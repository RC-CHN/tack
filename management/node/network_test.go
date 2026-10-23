package node

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	p "tack.local/management/protocol"
)

func testConfig(t *testing.T) p.NodeConfig {
	t.Helper()
	return p.NodeConfig{Server: "https://controller.invalid", NodeID: "node-a", Token: strings.Repeat("x", 40), StateDir: t.TempDir(), WorkDir: t.TempDir()}
}
func TestHeartbeatDefaultsAndBounds(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	if a.config.HeartbeatSeconds != 1800 || a.config.LeaseSeconds != 9000 {
		t.Fatalf("defaults: %+v", a.config)
	}
	for i := 0; i < 1000; i++ {
		n := heartbeatWait(1800)
		if n < 900 || n > 2700 {
			t.Fatal(n)
		}
	}
	cfg := testConfig(t)
	cfg.HeartbeatSeconds = 1
	if _, err := New(cfg); err == nil {
		t.Fatal("invalid heartbeat accepted")
	}
	cfg.HeartbeatSeconds = 1800
	cfg.LeaseSeconds = 2700
	if _, err := New(cfg); err == nil {
		t.Fatal("lease shorter than longest wait accepted")
	}
}
func TestV1RequestDoesNotContainV2Fields(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	a.version = p.Version
	a.client = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		for _, key := range []string{"metrics", "wait_seconds", "lease_seconds", "accept_task", "lease_expires_at", "upload"} {
			if strings.Contains(string(b), key) {
				t.Errorf("v2 field %s in v1 request: %s", key, b)
			}
		}
		return response(p.SyncResponse{Version: p.Version}), nil
	})}
	if _, err := a.sync(context.Background(), nil, p.NodeMetrics{}); err != nil {
		t.Fatal(err)
	}
}
func TestResultBackpressureReservesNextOutput(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	a.config.MaxPendingResults = 2
	a.config.MaxPendingResultBytes = 2 * p.MaxOutput
	records := map[string]journal{"one": {Report: p.Report{State: "succeeded", Result: &p.Result{Stdout: []byte("x")}}}}
	if a.nodeMetrics(records, "").AcceptingTasks {
		t.Fatal("insufficient next-result byte reservation")
	}
	a.config.MaxPendingResultBytes = 4 * p.MaxOutput
	if !a.nodeMetrics(records, "").AcceptingTasks {
		t.Fatal("unexpected backpressure")
	}
	records["two"] = records["one"]
	if a.nodeMetrics(records, "").AcceptingTasks {
		t.Fatal("ignored result count limit")
	}
	delete(records, "two")
	if a.nodeMetrics(records, "active").AcceptingTasks {
		t.Fatal("accepted second simultaneous task")
	}
}
func TestUploadRateWaitIsCancellable(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	a.config.UploadBytesPerSecond = 1
	a.Logf = nil
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	a.uploadClient = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		var in p.ResultChunk
		if err := p.Decode(r.Body, &in); err != nil {
			t.Error(err)
		}
		calls++
		if in.Probe {
			return response(p.ChunkAck{Version: p.TransferVersion, TaskID: in.TaskID}), nil
		}
		cancel()
		return response(p.ChunkAck{Version: p.TransferVersion, TaskID: in.TaskID, Offset: p.MaxChunk}), nil
	})}
	start := time.Now()
	err = a.upload(ctx, journal{Task: p.Task{ID: "task"}, Report: p.Report{Result: &p.Result{Stdout: make([]byte, p.MaxChunk+1)}}})
	if !errors.Is(err, context.Canceled) || calls != 2 || time.Since(start) > time.Second {
		t.Fatalf("rate limit did not cancel: %v, calls %d", err, calls)
	}
}
func TestRestartedCompletedJournalIsNotExecuted(t *testing.T) {
	cfg := testConfig(t)
	marker := filepath.Join(t.TempDir(), "must-not-run")
	exe, _ := os.Executable()
	rec := journal{Task: p.Task{ID: "finished", NodeID: cfg.NodeID, Command: p.Command{Mode: "exec", Program: exe, Args: []string{"-test.run=^TestHelperExecution$"}, Env: map[string]string{"TACK_TEST_HELPER": "1", "TACK_TEST_MARKER": marker}}}, Report: p.Report{TaskID: "finished", State: "succeeded", Result: &p.Result{Stdout: []byte("saved")}}}
	if err := p.WriteJSON(filepath.Join(cfg.StateDir, "journal.json"), map[string]journal{"finished": rec}); err != nil {
		t.Fatal(err)
	}
	a, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	a.client = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/health" {
			return response(map[string]int{"protocol_version": 1}), nil
		}
		var in p.SyncRequest
		p.Decode(r.Body, &in)
		if in.Report == nil || in.Report.Result == nil || string(in.Report.Result.Stdout) != "saved" {
			t.Error("lost saved result")
		}
		cancel()
		return response(p.SyncResponse{Version: 1}), nil
	})}
	if err := a.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("completed task executed on restart")
	}
}

func TestMetadataBoundIsStableThroughJSON(t *testing.T) {
	value, truncated := boundedMetadata(strings.Repeat("界", 4000), 8192, false)
	if !truncated || len(value) > 8192 || !utf8.ValidString(value) || !strings.HasSuffix(value, " [truncated]") {
		t.Fatal("invalid metadata truncation")
	}
}

// Restoring a controller snapshot can replay a task whose metadata was already
// acknowledged. The node must report the retained terminal result again.
func TestReplayedPendingTaskResendsMetadata(t *testing.T) {
	cfg := testConfig(t)
	task := p.Task{ID: "pending", NodeID: cfg.NodeID, State: "dispatched"}
	saved := journal{Task: task, Report: p.Report{TaskID: task.ID, State: "succeeded", Result: &p.Result{Stdout: []byte("saved")}}, MetadataAcknowledged: true}
	if err := p.WriteJSON(filepath.Join(cfg.StateDir, "journal.json"), map[string]journal{task.ID: saved}); err != nil {
		t.Fatal(err)
	}
	a, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	a.Logf = nil
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	resubmitted := false
	a.uploadClient = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) { <-r.Context().Done(); return nil, r.Context().Err() })}
	a.client = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/health" {
			return response(map[string]int{"protocol_version": 2}), nil
		}
		var in p.SyncRequest
		if err := p.Decode(r.Body, &in); err != nil {
			t.Error(err)
		}
		if in.Report == nil {
			return response(p.SyncResponse{Version: 2, Task: &task}), nil
		}
		if in.Report.State != "succeeded" || in.Report.Upload == nil || in.Report.Upload.StdoutBytes != 5 || len(in.Report.Result.Stdout) != 0 {
			t.Errorf("invalid retained metadata: %+v", in.Report)
		}
		resubmitted = true
		cancel()
		return response(p.SyncResponse{Version: 2}), nil
	})}
	if err := a.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if !resubmitted {
		t.Fatal("metadata acknowledgement suppressed recovery report")
	}
}
