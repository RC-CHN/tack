//go:build linux

package integration_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	p "tack.local/management/protocol"
	"tack.local/testfixtures/faultnet"
	"tack.local/testfixtures/resource"
)

type footprintPoint struct {
	Process        resource.Sample `json:"process"`
	Network        faultnet.Stats  `json:"network"`
	StateBytes     int64           `json:"state_logical_bytes"`
	LogBytes       int64           `json:"diagnostic_bytes"`
	UploadFailures int64           `json:"upload_failures"`
}
type footprintPhase struct {
	Name    string           `json:"name"`
	Samples []footprintPoint `json:"samples"`
}

// Opt-in because this runs the actual (non-race) Node in a separate process.
// TACK_RESOURCE_SECONDS is the length of EACH phase, not the entire test.
func TestNodeLowFootprint(t *testing.T) {
	raw := os.Getenv("TACK_RESOURCE_SECONDS")
	if raw == "" {
		t.Skip("set TACK_RESOURCE_SECONDS=30 and TACK_NODE_BINARY for independent-process resource gates")
	}
	seconds, err := strconv.Atoi(raw)
	if err != nil || seconds < 3 || seconds > 86400 {
		t.Fatal("TACK_RESOURCE_SECONDS must be 3..86400")
	}
	binary := os.Getenv("TACK_NODE_BINARY")
	if binary == "" {
		t.Fatal("TACK_NODE_BINARY must name a non-race tack-node build")
	}
	binary, err = filepath.Abs(binary)
	if err != nil {
		t.Fatal(err)
	}
	info := nodeBuild(t, binary)
	hzText, err := exec.Command("getconf", "CLK_TCK").Output()
	if err != nil {
		t.Fatal(err)
	}
	hz, err := strconv.ParseFloat(strings.TrimSpace(string(hzText)), 64)
	if err != nil || hz <= 0 {
		t.Fatal("invalid clock tick rate")
	}
	maxRSS := uint64(32 * 1024 * 1024)
	if raw := os.Getenv("TACK_NODE_MAX_RSS_MIB"); raw != "" {
		n, e := strconv.Atoi(raw)
		if e != nil || n < 1 || n > 1024 {
			t.Fatal("invalid TACK_NODE_MAX_RSS_MIB")
		}
		maxRSS = uint64(n) * 1024 * 1024
	}
	h2 := os.Getenv("TACK_RESOURCE_HTTP2") == "1"
	mode := "output"
	stdout, stderr := bytes.Repeat([]byte("o"), 160*1024), []byte("separate stderr")
	if os.Getenv("TACK_RESOURCE_MAX_OUTPUT") == "1" {
		mode = "max-output"
		stdout, stderr = bytes.Repeat([]byte("o"), p.MaxOutput), bytes.Repeat([]byte("e"), p.MaxOutput)
	}
	var blocked atomic.Bool
	var uploadFailures, protocol2 atomic.Int64
	f := newNetworkFixtureHTTP2Timeout(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.ProtoMajor == 2 {
				protocol2.Add(1)
			}
			if blocked.Load() && r.URL.Path == p.NodeResultPath {
				uploadFailures.Add(1)
				http.Error(w, "injected upload outage", 503)
				return
			}
			next.ServeHTTP(w, r)
		})
	}, h2, 20*time.Second)
	link, err := faultnet.New(strings.TrimPrefix(f.nodeServer.URL, "https://"))
	if err != nil {
		t.Fatal(err)
	}
	defer link.Close()
	f.nodeConfig.Server = "https://" + link.Addr()
	f.nodeConfig.MaxPendingResults = 2
	f.nodeConfig.MaxPendingResultBytes = 8 * 1024 * 1024
	config := filepath.Join(t.TempDir(), "node.json")
	if err = p.WriteJSON(config, f.nodeConfig); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "node.log")
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	cmd := exec.Command(binary, "-config", config)
	cmd.Stdout = log
	cmd.Stderr = log
	if err = cmd.Start(); err != nil {
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
	await := func(description string, check func() bool) {
		t.Helper()
		deadline := time.Now().Add(90 * time.Second)
		for time.Now().Before(deadline) {
			if check() {
				return
			}
			select {
			case e := <-done:
				stopped = true
				b, _ := os.ReadFile(logPath)
				t.Fatalf("Node exited while %s: %v %s", description, e, b)
			default:
			}
			time.Sleep(25 * time.Millisecond)
		}
		t.Fatal("timeout: " + description)
	}
	readTask := func(id string) p.Task { var task p.Task; f.request("GET", "/v1/tasks/"+id, nil, &task); return task }
	complete := func(id string) {
		await("complete "+id, func() bool { task := readTask(id); return task.State == "succeeded" && !task.ResultPending })
	}
	await("node registration", func() bool {
		var m p.MetricsSnapshot
		f.request("GET", "/v1/metrics", nil, &m)
		return len(m.Nodes) > 0 && m.Nodes[0].Metrics != nil
	})
	warm := f.submit("output", filepath.Join(t.TempDir(), "warm"))
	complete(warm.ID)
	await("warm result reclaimed", func() bool {
		var m p.MetricsSnapshot
		f.request("GET", "/v1/metrics", nil, &m)
		return m.Nodes[0].Metrics != nil && m.Nodes[0].Metrics.JournalRecords == 0
	})
	time.Sleep(200 * time.Millisecond)
	var phases []footprintPhase
	var recoveryMS, shutdownMS float64
	sample := func() footprintPoint {
		s, e := resource.Process(cmd.Process.Pid)
		if e != nil {
			t.Fatal(e)
		}
		var state int64
		e = filepath.WalkDir(f.nodeConfig.StateDir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() {
				i, e := d.Info()
				if os.IsNotExist(e) {
					return nil
				}
				if e != nil {
					return e
				}
				state += i.Size()
			}
			return nil
		})
		if e != nil {
			t.Fatal(e)
		}
		l, e := os.Stat(logPath)
		if e != nil {
			t.Fatal(e)
		}
		return footprintPoint{s, link.Snapshot(), state, l.Size(), uploadFailures.Load()}
	}
	// Write evidence even after a gate failure. A failed report is never labelled
	// a qualification pass; duration, RSS limit and toolchain are embedded in it.
	defer func() {
		report := struct {
			Format         string           `json:"format"`
			Passed         bool             `json:"passed"`
			HTTP2          bool             `json:"http2"`
			TLSProfile     string           `json:"tls_profile"`
			TrafficShaping string           `json:"traffic_shaping"`
			Seconds        int              `json:"seconds_per_phase"`
			ClockHz        float64          `json:"clock_ticks_per_second"`
			MaxRSS         uint64           `json:"rss_limit_bytes"`
			NodeGo         string           `json:"node_go_version"`
			Build          nodeBuildInfo    `json:"node_build"`
			OutputBytes    int              `json:"output_bytes_per_task"`
			RecoveryMS     float64          `json:"recovery_to_three_results_ms"`
			ShutdownMS     float64          `json:"shutdown_ms"`
			Phases         []footprintPhase `json:"phases"`
		}{"tack-footprint-v1", !t.Failed(), h2, f.nodeConfig.TLSProfile, f.nodeConfig.TrafficShaping, seconds, hz, maxRSS, info.GoVersion, info, len(stdout) + len(stderr), recoveryMS, shutdownMS, phases}
		b, e := json.MarshalIndent(report, "", "  ")
		if e != nil {
			t.Error(e)
			return
		}
		if path := os.Getenv("TACK_RESOURCE_REPORT"); path != "" {
			if e = os.WriteFile(path, append(b, '\n'), 0600); e != nil {
				t.Error(e)
			}
		}
	}()
	phase := func(name string, quiet bool) {
		t.Helper()
		ph := footprintPhase{Name: name, Samples: []footprintPoint{sample()}}
		start := ph.Samples[0]
		deadline := time.Now().Add(time.Duration(seconds) * time.Second)
		for time.Now().Before(deadline) {
			wait := time.Until(deadline)
			if wait > time.Second {
				wait = time.Second
			}
			time.Sleep(wait)
			s := sample()
			ph.Samples = append(ph.Samples, s)
			if s.Process.RSS > maxRSS || s.Process.PeakRSS > maxRSS {
				t.Errorf("%s RSS exceeds %d MiB: %+v", name, maxRSS>>20, s.Process)
			}
			if s.Process.FDs > start.Process.FDs+4 {
				t.Errorf("%s FD growth: %d -> %d", name, start.Process.FDs, s.Process.FDs)
			}
			if s.LogBytes != 0 {
				t.Errorf("routine diagnostic writes while %s: %d", name, s.LogBytes)
			}
		}
		phases = append(phases, ph)
		end := ph.Samples[len(ph.Samples)-1]
		elapsed := end.Process.At.Sub(start.Process.At).Seconds()
		cpu := float64(end.Process.CPUTicks-start.Process.CPUTicks) / hz
		if cpu > 0.20+elapsed*0.02 {
			t.Errorf("%s CPU %.3fs exceeds 0.20s + 2%% of %.3fs", name, cpu, elapsed)
		}
		if end.Process.WriteBytes-start.Process.WriteBytes > 64*1024 {
			t.Errorf("%s storage writes while work is unchanged: %d", name, end.Process.WriteBytes-start.Process.WriteBytes)
		}
		if end.StateBytes != start.StateBytes {
			t.Errorf("%s state grew without new work: %d -> %d", name, start.StateBytes, end.StateBytes)
		}
		if quiet {
			traffic := (end.Network.UpBytes - start.Network.UpBytes) + (end.Network.DownBytes - start.Network.DownBytes)
			if traffic > uint64(4096+seconds*16) {
				t.Errorf("%s idle stream traffic %d bytes", name, traffic)
			}
		}
		t.Logf("%s: RSS=%d..%d KiB peak=%d KiB CPU=%.3fs fds=%d..%d written=%d state=%d", name, start.Process.RSS/1024, end.Process.RSS/1024, end.Process.PeakRSS/1024, cpu, start.Process.FDs, end.Process.FDs, end.Process.WriteBytes-start.Process.WriteBytes, end.StateBytes)
	}
	phase("idle", true)
	baseline := phases[0].Samples[0]
	blocked.Store(true)
	marker := filepath.Join(t.TempDir(), "executions")
	first := f.submit(mode, marker)
	second := f.submit(mode, marker)
	await("two retained results", func() bool {
		a, b := readTask(first.ID), readTask(second.ID)
		return a.State == "succeeded" && a.ResultPending && b.State == "succeeded" && b.ResultPending && uploadFailures.Load() > 0
	})
	third := f.submit(mode, marker)
	time.Sleep(200 * time.Millisecond) // Let terminal metadata acknowledgements settle.
	phase("upload_blocked", false)
	if state := readTask(third.ID).State; state != "queued" {
		t.Fatalf("backpressure admitted extra task: %s", state)
	}
	link.SetMode(faultnet.Offline)
	await("closed transport", func() bool { return link.Snapshot().Active == 0 })
	phase("offline", true)
	offline := phases[len(phases)-1]
	attempts := offline.Samples[len(offline.Samples)-1].Network.Accepted - offline.Samples[0].Network.Accepted
	// Both control and upload can retry; even their shortest initial jitter
	// cannot justify sustained busy retrying. The base doubles to a 30s cap.
	if attempts > uint64(8+seconds/5*2) {
		t.Errorf("reconnect storm: %d connections in %ds", attempts, seconds)
	}
	blocked.Store(false)
	recoveryStart := time.Now()
	link.SetMode(faultnet.Online)
	for _, id := range []string{first.ID, second.ID, third.ID} {
		complete(id)
		result := readTask(id).Result
		if result == nil || !bytes.Equal(result.Stdout, stdout) || !bytes.Equal(result.Stderr, stderr) {
			t.Fatal("recovered output mismatch")
		}
	}
	recoveryMS = float64(time.Since(recoveryStart)) / float64(time.Millisecond)
	data, err := os.ReadFile(marker)
	if err != nil || string(data) != "xxx" {
		t.Fatalf("commands reran or vanished: %q %v", data, err)
	}
	await("reclaimed results", func() bool {
		var m p.MetricsSnapshot
		f.request("GET", "/v1/metrics", nil, &m)
		return m.Nodes[0].Metrics != nil && m.Nodes[0].Metrics.JournalRecords == 0
	})
	time.Sleep(200 * time.Millisecond)
	phase("recovered_idle", true)
	after := sample()
	if after.Process.RSS > baseline.Process.RSS+8*1024*1024 || after.Process.FDs > baseline.Process.FDs+2 {
		t.Errorf("resources did not settle: baseline=%+v after=%+v", baseline.Process, after.Process)
	}
	if h2 && protocol2.Load() == 0 {
		t.Error("HTTP/2 not exercised")
	}
	start := time.Now()
	if err = cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-done:
		stopped = true
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Node did not shut down with long poll pending")
	}
	shutdownMS = float64(time.Since(start)) / float64(time.Millisecond)
	t.Logf("shutdown=%.3fms recovery_to_three_results=%.3fms attempts during outage=%d HTTP/2 requests=%d", shutdownMS, recoveryMS, attempts, protocol2.Load())
}
