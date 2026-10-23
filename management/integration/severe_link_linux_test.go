//go:build linux

package integration_test

import (
	"bytes"
	"debug/buildinfo"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	p "tack.local/management/protocol"
	"tack.local/testfixtures/faultnet"
	"tack.local/testfixtures/resource"
)

// A fixed-duration qualification, including incomplete transfers in the report.
// The controller, command child and fault proxy are outside the measured Node.
func TestNodeSevereLink(t *testing.T) {
	if os.Getenv("TACK_SEVERE_SECONDS") == "" {
		t.Skip("set TACK_SEVERE_SECONDS and TACK_NODE_BINARY")
	}
	seconds, err := strconv.Atoi(os.Getenv("TACK_SEVERE_SECONDS"))
	if err != nil || seconds < 20 || seconds > 600 {
		t.Fatal("TACK_SEVERE_SECONDS must be 20..600")
	}
	binary, err := filepath.Abs(os.Getenv("TACK_NODE_BINARY"))
	if err != nil {
		t.Fatal(err)
	}
	build, err := buildinfo.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range build.Settings {
		if s.Key == "-race" && s.Value == "true" {
			t.Fatal("Node must not use race instrumentation")
		}
	}
	h2 := os.Getenv("TACK_RESOURCE_HTTP2") == "1"
	first := make(chan struct{})
	gate := make(chan struct{})
	var firstOnce, gateOnce sync.Once
	unblock := func() { gateOnce.Do(func() { close(gate) }) }
	defer unblock()
	var requests atomic.Int64
	type attempt struct {
		At    time.Time `json:"at"`
		Bytes int64     `json:"request_content_length"`
	}
	var attemptMu sync.Mutex
	var attempts []attempt
	f := newNetworkFixtureHTTP2Timeout(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == p.NodeResultPath {
				requests.Add(1)
				attemptMu.Lock()
				if len(attempts) < 128 {
					attempts = append(attempts, attempt{time.Now().UTC(), r.ContentLength})
				}
				attemptMu.Unlock()
				firstOnce.Do(func() { close(first) })
				select {
				case <-gate:
				case <-r.Context().Done():
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}, h2, 20*time.Second)
	// 600 ms added round-trip delay, 8 KiB/s each direction, bounded 8 KiB
	// resident payload per direction. Real packet loss can be layered with netem.
	link, err := faultnet.NewConfigured(strings.TrimPrefix(f.nodeServer.URL, "https://"), faultnet.Options{Upload: faultnet.Traffic{Delay: 300 * time.Millisecond, BytesPerSecond: 8192}, Download: faultnet.Traffic{Delay: 300 * time.Millisecond, BytesPerSecond: 8192}, QueueChunks: 2, ChunkBytes: 2048})
	if err != nil {
		t.Fatal(err)
	}
	defer link.Close()
	f.nodeConfig.Server = "https://" + link.Addr()
	config := filepath.Join(t.TempDir(), "node.json")
	if err := p.WriteJSON(config, f.nodeConfig); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "node.log")
	log, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	cmd := exec.Command(binary, "-config", config)
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	stopped := false
	defer func() {
		if !stopped {
			cmd.Process.Kill()
			<-done
		}
	}()
	marker := filepath.Join(t.TempDir(), "executed")
	task := f.submit("severe-output", marker)
	select {
	case <-first:
	case <-time.After(40 * time.Second):
		t.Fatal("Node never reached upload probe")
	}
	// Observe the Controller in-process, outside the impaired network. Observation
	// must neither compete for link bandwidth nor introduce another TCP timeout.
	readTask := func() p.Task {
		r := httptest.NewRequest("GET", "/v1/tasks/"+task.ID, nil)
		r.Header.Set("Authorization", "Bearer "+f.config.AdminToken)
		w := httptest.NewRecorder()
		f.api.AdminHandler().ServeHTTP(w, r)
		var out p.Task
		if w.Code != 200 {
			t.Fatalf("task observation: HTTP %d", w.Code)
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	var samples []resource.Sample
	sample := func() resource.Sample {
		s, err := resource.Process(cmd.Process.Pid)
		if err != nil {
			t.Fatal(err)
		}
		samples = append(samples, s)
		return s
	}
	startSample := sample()
	startNetwork := link.Snapshot()
	start := time.Now()
	unblock()
	stopFaults := make(chan struct{})
	faultsDone := make(chan struct{})
	var outages atomic.Int64
	go func() {
		defer close(faultsDone)
		ticker := time.NewTicker(4 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stopFaults:
				return
			case <-ticker.C:
			}
			link.SetMode(faultnet.Offline)
			outages.Add(1)
			timer := time.NewTimer(500 * time.Millisecond)
			select {
			case <-stopFaults:
				timer.Stop()
				return
			case <-timer.C:
			}
			link.SetMode(faultnet.Online)
		}
	}()
	var stopOnce sync.Once
	stop := func() { stopOnce.Do(func() { close(stopFaults); <-faultsDone }) }
	defer stop()
	completed, progress := false, int64(0)
	for time.Since(start) < time.Duration(seconds)*time.Second {
		current := readTask()
		if current.State == "succeeded" && !current.ResultPending {
			if current.Result == nil || !bytes.Equal(current.Result.Stdout, bytes.Repeat([]byte{'o'}, 32*1024)) || string(current.Result.Stderr) != "separate stderr" {
				t.Fatal("corrupt severe-link result")
			}
			completed, progress = true, int64(32*1024+len("separate stderr"))
			break
		}
		if current.Upload != nil {
			path := filepath.Join(f.statePath+".uploads", task.ID+"."+current.Upload.SHA256+".part")
			if info, err := os.Stat(path); err == nil && info.Size() > progress {
				progress = info.Size()
			}
		}
		sample()
		time.Sleep(250 * time.Millisecond)
	}
	stop()
	endSample := sample()
	endNetwork := link.Snapshot()
	elapsed := time.Since(start).Seconds()
	data, err := os.ReadFile(marker)
	if err != nil || string(data) != "x" {
		t.Fatal("command reran or vanished", string(data), err)
	}
	shutdown := time.Now()
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		stopped = true
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown blocked")
	}
	shutdownMS := float64(time.Since(shutdown)) / float64(time.Millisecond)
	logInfo, err := os.Stat(logPath)
	if err != nil {
		t.Fatal(err)
	}
	maxRSS, maxFD := uint64(0), 0
	for _, s := range samples {
		maxRSS = max(maxRSS, s.RSS, s.PeakRSS)
		maxFD = max(maxFD, s.FDs)
	}
	if maxRSS > 32<<20 || maxFD > startSample.FDs+4 || logInfo.Size() != 0 {
		t.Error("resource gate failed", maxRSS, maxFD, logInfo.Size())
	}
	if os.Getenv("TACK_SEVERE_REQUIRE_COMPLETE") == "1" && !completed {
		t.Error("result did not complete within severe-link budget")
	}
	attemptMu.Lock()
	attemptsCopy := append([]attempt(nil), attempts...)
	attemptMu.Unlock()
	report := map[string]any{
		"format": "tack-severe-link-v1", "passed": !t.Failed(), "completed": completed,
		"budget_seconds": seconds, "elapsed_seconds": elapsed, "http2": h2,
		"node_go": build.GoVersion, "node_binary": binary, "max_rss_bytes": maxRSS, "max_fds": maxFD,
		"start": startSample, "end": endSample, "samples": samples,
		"network_start": startNetwork, "network_end": endNetwork,
		"output_bytes": 32*1024 + len("separate stderr"), "progress_bytes": progress,
		"upload_requests": requests.Load(), "outages": outages.Load(), "shutdown_ms": shutdownMS,
		"attempts":  attemptsCopy,
		"log_bytes": logInfo.Size(), "rate_bytes_per_second": 8192, "one_way_delay_ms": 300,
		"outage_interval_ms": 4000, "outage_duration_ms": 500,
	}
	if path := os.Getenv("TACK_SEVERE_REPORT"); path != "" {
		data, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(data, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("completed=%v progress=%d/%d elapsed=%.3fs requests=%d outages=%d stream=%dB maxRSS=%.2fMiB shutdown=%.2fms", completed, progress, 32*1024+len("separate stderr"), elapsed, requests.Load(), outages.Load(), endNetwork.UpBytes-startNetwork.UpBytes+endNetwork.DownBytes-startNetwork.DownBytes, float64(maxRSS)/(1<<20), shutdownMS)
}
