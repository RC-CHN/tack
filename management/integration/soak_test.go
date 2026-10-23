package integration_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	p "tack.local/management/protocol"
)

type resourceSample struct {
	At          time.Time `json:"at"`
	HeapBytes   uint64    `json:"heap_bytes_after_gc"`
	HeapObjects uint64    `json:"heap_objects_after_gc"`
	RSSBytes    int64     `json:"rss_bytes"`
	Goroutines  int       `json:"goroutines"`
	FDs         int       `json:"file_descriptors"`
}

func sampleResources() resourceSample {
	// Two cycles also age out sync.Pool buffers. RSS is reported separately;
	// an allocator's retained pages are not by themselves proof of a leak.
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	s := resourceSample{At: time.Now().UTC(), HeapBytes: m.HeapAlloc, HeapObjects: m.HeapObjects, Goroutines: runtime.NumGoroutine(), FDs: -1, RSSBytes: -1}
	if runtime.GOOS == "linux" {
		if entries, err := os.ReadDir("/proc/self/fd"); err == nil {
			s.FDs = len(entries)
		}
		if data, err := os.ReadFile("/proc/self/status"); err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				fields := strings.Fields(line)
				if len(fields) >= 2 && fields[0] == "VmRSS:" {
					kb, _ := strconv.ParseInt(fields[1], 10, 64)
					s.RSSBytes = kb * 1024
				}
			}
		}
	}
	return s
}
func checkResourceGrowth(t *testing.T, baseline, current resourceSample) {
	t.Helper()
	// This catches coarse accumulation, not arbitrarily small leaks. The fixed
	// task history is held constant throughout this phase.
	if int64(current.HeapBytes)-int64(baseline.HeapBytes) > 8*1024*1024 {
		t.Errorf("retained heap grew by >8 MiB: baseline=%+v current=%+v", baseline, current)
	}
	if current.Goroutines > baseline.Goroutines+8 {
		t.Errorf("goroutine accumulation: baseline=%+v current=%+v", baseline, current)
	}
	if baseline.FDs >= 0 && current.FDs > baseline.FDs+8 {
		t.Errorf("file descriptor accumulation: baseline=%+v current=%+v", baseline, current)
	}
}

// Opt-in: real task execution followed by a fixed-history reconnect soak. Both
// controller and node run in this test process, so resource measurements are
// combined. Use -timeout longer than the requested duration plus setup time.
func TestManagementResourceSoak(t *testing.T) {
	raw := os.Getenv("TACK_SOAK_SECONDS")
	if raw == "" {
		t.Skip("set TACK_SOAK_SECONDS (>=30) for the resource soak")
	}
	seconds, err := strconv.Atoi(raw)
	if err != nil || seconds < 30 || seconds > 7*24*3600 {
		t.Fatal("TACK_SOAK_SECONDS must be 30..604800")
	}
	tasks := 64
	if raw := os.Getenv("TACK_SOAK_TASKS"); raw != "" {
		tasks, err = strconv.Atoi(raw)
		if err != nil || tasks < 1 || tasks > 1000 {
			t.Fatal("TACK_SOAK_TASKS must be 1..1000")
		}
	}
	h2 := os.Getenv("TACK_SOAK_HTTP2") == "1"
	var churn atomic.Bool
	var controls, uploads, drops, unavailable, protocol2 atomic.Int64
	f := newNetworkFixtureHTTP2(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.ProtoMajor == 2 {
				protocol2.Add(1)
			}
			if r.URL.Path == p.NodeResultPath {
				if uploads.Add(1)%13 == 0 {
					unavailable.Add(1)
					http.Error(w, "injected upload failure", 503)
					return
				}
			}
			if r.URL.Path == p.NodeRetentionPath {
				seq := controls.Add(1)
				if churn.Load() {
					switch seq % 4 {
					case 1:
						drops.Add(1)
						panic(http.ErrAbortHandler)
					case 3:
						unavailable.Add(1)
						http.Error(w, "injected control failure", 503)
						return
					default:
						// A test proxy shortens only successful idle waits, resetting retry
						// backoff between faults without changing production heartbeat defaults.
						var in p.SyncRequest
						if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
							t.Error(err)
							return
						}
						r.Body.Close()
						in.WaitSeconds = 0
						body, _ := json.Marshal(in)
						r.Body = io.NopCloser(bytes.NewReader(body))
						r.ContentLength = int64(len(body))
					}
				}
			}
			next.ServeHTTP(w, r)
		})
	}, h2)
	f.nodeConfig.HeartbeatSeconds = 2
	stop := f.start()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "executions")
	for i := 0; i < tasks; i++ {
		in := p.CreateTask{NodeID: "node-a", Command: p.Command{Mode: "exec", Program: exe, Args: []string{"-test.run=^TestOutputHelper$"}, Env: map[string]string{"TACK_WEAKNET_HELPER": "output", "TACK_WEAKNET_MARKER": marker, "TACK_SOAK_PADDING": strings.Repeat("x", 8192), "GORACE": "atexit_sleep_ms=0"}, TimeoutSeconds: 20, OutputLimit: 2048}}
		var task p.Task
		f.request("POST", "/v1/tasks", in, &task)
		final := f.task(task.ID, func(task p.Task) bool { return p.Terminal(task.State) && !task.ResultPending })
		if final.State != "succeeded" || final.Result == nil || len(final.Result.Stdout) != 2048 || !final.Result.StdoutTruncated {
			t.Fatalf("task %d did not execute and bound output: %+v", i, final)
		}
		if (i+1)%16 == 0 {
			t.Logf("executed and uploaded %d/%d tasks", i+1, tasks)
		}
	}
	f.metrics(func(m p.MetricsSnapshot) bool {
		return m.Nodes[0].Metrics != nil && m.Nodes[0].Metrics.PendingResults == 0 && m.Nodes[0].Metrics.JournalRecords == 0
	})
	data, err := os.ReadFile(marker)
	if err != nil || len(data) != tasks {
		t.Fatalf("execution count=%d want=%d err=%v", len(data), tasks, err)
	}
	journal, err := os.ReadFile(filepath.Join(f.nodeConfig.StateDir, "journal.json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(journal, []byte("TACK_SOAK_PADDING")) || len(journal) > tasks*2048+4096 {
		t.Fatal("acknowledged journal retained command payloads")
	}
	work, err := filepath.Glob(filepath.Join(f.nodeConfig.WorkDir, "tack-*", "task-*"))
	if err != nil || len(work) != 0 {
		t.Fatalf("task workspaces not cleaned: count=%d err=%v", len(work), err)
	}
	state, err := os.Stat(f.statePath)
	if err != nil {
		t.Fatal(err)
	}
	churn.Store(true)
	f.nodeServer.CloseClientConnections()
	warm := controls.Load()
	eventually(t, "warm reconnects", func() bool { return controls.Load() >= warm+8 })
	baseline := sampleResources()
	samples := []resourceSample{baseline}
	deadline := time.Now().Add(time.Duration(seconds) * time.Second)
	for time.Now().Before(deadline) {
		wait := time.Until(deadline)
		if wait > 15*time.Second {
			wait = 15 * time.Second
		}
		time.Sleep(wait)
		current := sampleResources()
		samples = append(samples, current)
		checkResourceGrowth(t, baseline, current)
		t.Logf("steady sample: heap=%d KiB rss=%d KiB goroutines=%d fd=%d control_requests=%d", current.HeapBytes/1024, current.RSSBytes/1024, current.Goroutines, current.FDs, controls.Load())
	}
	stop()
	if h2 && protocol2.Load() == 0 {
		t.Error("HTTP/2 was requested but never negotiated")
	}
	if drops.Load() < 5 {
		t.Error("too few reconnections exercised")
	}
	if info, err := os.Stat(f.statePath); err != nil || info.Size() != state.Size() {
		t.Error("fixed-history soak changed task state")
	}
	if info, err := os.Stat(filepath.Join(f.nodeConfig.StateDir, "journal.json")); err != nil || info.Size() != int64(len(journal)) {
		t.Error("fixed-history soak changed journal size")
	}
	f.nodeServer.Close()
	f.admin.Client().CloseIdleConnections()
	f.admin.Close()
	final := sampleResources()
	report := struct {
		Tasks           int              `json:"tasks"`
		Seconds         int              `json:"steady_seconds"`
		HTTP2           bool             `json:"http2"`
		Controls        int64            `json:"control_requests"`
		Uploads         int64            `json:"upload_requests"`
		Drops           int64            `json:"connection_aborts"`
		Unavailable     int64            `json:"http_503"`
		JournalBytes    int              `json:"node_journal_bytes"`
		ControllerBytes int64            `json:"controller_state_bytes"`
		Samples         []resourceSample `json:"samples"`
		AfterClose      resourceSample   `json:"after_close"`
	}{tasks, seconds, h2, controls.Load(), uploads.Load(), drops.Load(), unavailable.Load(), len(journal), state.Size(), samples, final}
	if path := os.Getenv("TACK_SOAK_REPORT"); path != "" {
		b, _ := json.MarshalIndent(report, "", "  ")
		if err := os.WriteFile(path, append(b, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("tasks=%d controls=%d uploads=%d connection_aborts=%d http503=%d journal=%d controller_state=%d after_close=%+v", tasks, report.Controls, report.Uploads, report.Drops, report.Unavailable, report.JournalBytes, report.ControllerBytes, final)
}
