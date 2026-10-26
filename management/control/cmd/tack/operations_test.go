package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	p "tack.local/management/protocol"
)

func TestPrepareSubmissionPersistsBeforeRetry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task.json")
	if err := p.WriteJSON(path, p.CreateTask{NodeID: "node-a", Command: p.Command{Mode: "exec", Program: "echo"}}); err != nil {
		t.Fatal(err)
	}
	first, err := prepareSubmission(path, "", "batch-one", "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := prepareSubmission(path, "", "", "")
	if err != nil || second.RequestID != first.RequestID || second.CallerID != "batch-one" {
		t.Fatal("retry lost identity", err)
	}
	var persisted p.CreateTask
	if err := p.ReadJSON(path, &persisted); err != nil || persisted.RequestID != first.RequestID {
		t.Fatal("identity not durable")
	}
	different, _ := p.NewRequestID()
	if _, err := prepareSubmission(path, different, "", ""); err == nil {
		t.Fatal("replaced existing identity")
	}
}
func TestWaitIncludesOutputAndNeverCancels(t *testing.T) {
	var calls atomic.Int32
	var complete atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || strings.Contains(r.URL.Path, "cancel") {
			t.Error("wait sent a mutation")
		}
		calls.Add(1)
		json.NewEncoder(w).Encode(p.Task{ID: "task", State: "succeeded", ResultPending: !complete.Load(), Result: &p.Result{Stdout: []byte("ok")}})
	}))
	defer srv.Close()
	api := apiClient{client: srv.Client(), origin: srv.URL}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := waitTask(ctx, api, "/v1/tasks/task", time.Millisecond)
	if exitCode(err) != 4 || calls.Load() < 2 {
		t.Fatal("wait did not wait for output", err, calls.Load())
	}
	complete.Store(true)
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	b, err := waitTask(ctx2, api, "/v1/tasks/task", time.Millisecond)
	if err != nil || !bytes.Contains(b, []byte("succeeded")) {
		t.Fatal(err)
	}
}
func TestDecodedResultsAndExactOutputFiles(t *testing.T) {
	zero := 0
	task := p.Task{ID: "task", State: "succeeded", Result: &p.Result{ExitCode: &zero, Stdout: []byte{0, 255, 'a'}, Stderr: []byte("error\n"), StdoutTruncated: true, Cleanup: &p.CleanupResult{ErrorCode: "cleanup_failed", Path: "/work/retained", Error: "busy"}}}
	raw, _ := json.Marshal(task)
	out, stderr := filepath.Join(t.TempDir(), "out"), filepath.Join(t.TempDir(), "err")
	var b bytes.Buffer
	if err := renderTask(&b, raw, "json", out, stderr); err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(b.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	result := decoded["result"].(map[string]any)
	if result["stdout_text_lossy"] != true || result["stderr"] != "error\n" {
		t.Fatal(b.String())
	}
	saved, _ := os.ReadFile(out)
	if !bytes.Equal(saved, task.Result.Stdout) {
		t.Fatal("raw bytes changed")
	}
	saved, _ = os.ReadFile(stderr)
	if !bytes.Equal(saved, task.Result.Stderr) {
		t.Fatal("stderr changed")
	}
	b.Reset()
	if err := renderTask(&b, raw, "text", "", ""); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"exit_code: 0", "stdout_truncated: true", "cleanup_error: cleanup_failed", "cleanup_path: /work/retained"} {
		if !strings.Contains(b.String(), expected) {
			t.Fatal("missing", expected)
		}
	}
	task.ResultPending = true
	raw, _ = json.Marshal(task)
	if err := renderTask(&b, raw, "json", out, stderr); err == nil {
		t.Fatal("saved incomplete output")
	}
}
func TestGetQuerySemanticsAndWaitCheckExit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(p.Task{ID: "task", State: "failed", Result: &p.Result{ErrorCode: "command_exit_nonzero"}})
	}))
	defer srv.Close()
	cred := filepath.Join(t.TempDir(), "credential.json")
	p.WriteJSON(cred, p.ClientCredential{Token: strings.Repeat("x", 40)})
	common := []string{"--server", srv.URL, "--credential", cred, "--allow-insecure-loopback", "--id", "task"}
	if err := run(append([]string{"get"}, common...)); err != nil {
		t.Fatal("get interpreted failed command as failed query", err)
	}
	if err := run(append(append([]string{"wait"}, common...), "--check")); exitCode(err) != 3 {
		t.Fatal("wait check", err)
	}
	if err := run(append(append([]string{"get"}, common...), "--check")); err == nil {
		t.Fatal("get accepted check flag")
	}
}

func TestWaitBudgetBoundsStalledQuery(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := waitTask(ctx, apiClient{client: srv.Client(), origin: srv.URL}, "/v1/tasks/task", time.Second)
	if exitCode(err) != 4 || time.Since(start) > time.Second {
		t.Fatal("query exceeded wait budget", err)
	}
}
