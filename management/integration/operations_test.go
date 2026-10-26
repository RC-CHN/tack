package integration_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	p "tack.local/management/protocol"
)

func TestOperationsRetryAndUploadDiagnostics(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	f := newNetworkFixture(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == p.NodeResultPath && fail.Load() {
				http.Error(w, "temporary disk failure", 503)
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	f.start()
	marker := filepath.Join(t.TempDir(), "once")
	exe, _ := os.Executable()
	id, _ := p.NewRequestID()
	in := p.CreateTask{NodeID: "node-a", RequestID: id, CallerID: "agent-batch", Command: p.Command{Mode: "exec", Program: exe, Args: []string{"-test.run=^TestOutputHelper$"}, Env: map[string]string{"TACK_WEAKNET_HELPER": "output", "TACK_WEAKNET_MARKER": marker, "GORACE": "atexit_sleep_ms=0"}, TimeoutSeconds: 40, OutputLimit: 200 * 1024}}
	var first, retried p.Task
	f.request("POST", "/v1/tasks", in, &first)
	f.request("POST", "/v1/tasks", in, &retried)
	if first.ID != retried.ID {
		t.Fatal("submission replay created a new task")
	}
	pending := f.task(first.ID, func(task p.Task) bool {
		return task.ResultPending && task.Transfer != nil && task.Transfer.ErrorCode == "result_upload_failed"
	})
	if pending.State != "succeeded" || pending.Result.ErrorCode != "" || pending.Result.ExitCode == nil || *pending.Result.ExitCode != 0 {
		t.Fatalf("upload failure contaminated execution: %+v", pending)
	}
	fail.Store(false)
	done := f.task(first.ID, func(task p.Task) bool { return task.State == "succeeded" && !task.ResultPending })
	if done.Transfer != nil || len(done.Result.Stdout) != 160*1024 {
		t.Fatal("incomplete result or stale upload diagnosis")
	}
	f.request("POST", "/v1/tasks", in, &retried)
	data, err := os.ReadFile(marker)
	if err != nil || !bytes.Equal(data, []byte("x")) || retried.ID != first.ID {
		t.Fatalf("duplicate execution: %q %v", data, err)
	}
}

func TestOfflineExpiredQueueNeverExecutes(t *testing.T) {
	f := newNetworkFixture(t, nil)
	marker := filepath.Join(t.TempDir(), "must-not-exist")
	exe, _ := os.Executable()
	id, _ := p.NewRequestID()
	before := time.Now().Add(2 * time.Second)
	in := p.CreateTask{NodeID: "node-a", RequestID: id, StartBefore: &before, Command: p.Command{Mode: "exec", Program: exe, Args: []string{"-test.run=^TestOutputHelper$"}, Env: map[string]string{"TACK_WEAKNET_HELPER": "output", "TACK_WEAKNET_MARKER": marker}, TimeoutSeconds: 40, OutputLimit: 100}}
	var task p.Task
	f.request("POST", "/v1/tasks", in, &task)
	time.Sleep(time.Until(before) + 20*time.Millisecond)
	f.start()
	got := f.task(task.ID, func(task p.Task) bool { return task.State == "expired" })
	if got.Result.ErrorCode != "start_deadline_exceeded" || got.Result.ExitCode != nil {
		t.Fatal("invalid expiration result")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("expired task executed", err)
	}
}

func TestV4NodeChecksDeadlineAfterDispatchTransit(t *testing.T) {
	var delayed atomic.Bool
	f := newNetworkFixture(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != p.NodeOperationsPath {
				next.ServeHTTP(w, r)
				return
			}
			// Delay only a dispatch response: controller expiry alone cannot protect
			// against a command that crosses its start deadline in transit.
			capture := newResponseCapture(w)
			next.ServeHTTP(capture, r)
			var out p.SyncResponse
			if json.Unmarshal(capture.Body.Bytes(), &out) == nil && out.Task != nil && out.Task.StartBefore != nil && !delayed.Swap(true) {
				time.Sleep(time.Until(*out.Task.StartBefore) + 20*time.Millisecond)
			}
			capture.flush()
		})
	})
	f.start()
	// Wait for initial replay fencing before submitting the task.
	f.metrics(func(m p.MetricsSnapshot) bool {
		return len(m.Nodes) > 0 && m.Nodes[0].Status != nil && m.Nodes[0].Status.AcceptingTasks
	})
	exe, _ := os.Executable()
	marker := filepath.Join(t.TempDir(), "never")
	deadline := time.Now().Add(2 * time.Second)
	id, _ := p.NewRequestID()
	in := p.CreateTask{NodeID: "node-a", RequestID: id, StartBefore: &deadline, Command: p.Command{Mode: "exec", Program: exe, Args: []string{"-test.run=^TestOutputHelper$"}, Env: map[string]string{"TACK_WEAKNET_HELPER": "output", "TACK_WEAKNET_MARKER": marker}, TimeoutSeconds: 5, OutputLimit: 100}}
	var task p.Task
	f.request("POST", "/v1/tasks", in, &task)
	f.task(task.ID, func(task p.Task) bool { return task.State == "expired" && !task.ResultPending })
	if !delayed.Load() {
		t.Fatal("did not exercise dispatched deadline")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("late task executed", err)
	}
}

type responseCapture struct {
	*httptest.ResponseRecorder
	underlying http.ResponseWriter
}

func newResponseCapture(w http.ResponseWriter) *responseCapture {
	return &responseCapture{httptest.NewRecorder(), w}
}
func (c *responseCapture) Unwrap() http.ResponseWriter { return c.underlying }
func (c *responseCapture) flush() {
	for k, v := range c.Header() {
		c.underlying.Header()[k] = v
	}
	c.underlying.WriteHeader(c.Code)
	c.underlying.Write(c.Body.Bytes())
}
