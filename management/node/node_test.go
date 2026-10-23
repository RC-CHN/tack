package node

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	p "tack.local/management/protocol"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func response(v any) *http.Response {
	b, _ := json.Marshal(v)
	return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(b))}
}

func TestHelperExecution(t *testing.T) {
	if os.Getenv("TACK_TEST_HELPER") != "1" {
		return
	}
	f, err := os.OpenFile(os.Getenv("TACK_TEST_MARKER"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		os.Exit(2)
	}
	f.WriteString("x")
	f.Close()
	os.Exit(0)
}

func TestRepeatedDeliveryDoesNotExecuteTwice(t *testing.T) {
	config := p.NodeConfig{Server: "https://controller.invalid", NodeID: "node-a", Token: strings.Repeat("x", 40), StateDir: t.TempDir(), WorkDir: t.TempDir(), PollSeconds: 1}
	a, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "marker")
	task := p.Task{ID: "test-task", NodeID: config.NodeID, State: "dispatched", Command: p.Command{Mode: "exec", Program: executable, Args: []string{"-test.run=TestHelperExecution"}, Env: map[string]string{"TACK_TEST_HELPER": "1", "TACK_TEST_MARKER": marker}, TimeoutSeconds: 10, OutputLimit: 100}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	terminalReports := 0
	a.client = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/health" {
			return response(map[string]int{"protocol_version": 1}), nil
		}
		var input p.SyncRequest
		if err := p.Decode(r.Body, &input); err != nil {
			t.Fatal(err)
		}
		if input.Report != nil && p.Terminal(input.Report.State) {
			terminalReports++
			if terminalReports == 2 {
				cancel()
			}
		}
		return response(p.SyncResponse{Version: p.Version, Task: &task}), nil
	})}
	if err := a.Run(ctx); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "x" || terminalReports != 2 {
		t.Fatalf("executions %q, reports %d", b, terminalReports)
	}
}

func TestRestartMarksIntentUnknown(t *testing.T) {
	config := p.NodeConfig{Server: "https://controller.invalid", NodeID: "node-a", Token: strings.Repeat("x", 40), StateDir: t.TempDir(), WorkDir: t.TempDir(), PollSeconds: 1}
	records := map[string]journal{"interrupted": {Task: p.Task{ID: "interrupted", NodeID: config.NodeID}, Report: p.Report{TaskID: "interrupted", State: "running"}}}
	if err := p.WriteJSON(filepath.Join(config.StateDir, "journal.json"), records); err != nil {
		t.Fatal(err)
	}
	a, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.client = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/health" {
			return response(map[string]int{"protocol_version": 1}), nil
		}
		var input p.SyncRequest
		p.Decode(r.Body, &input)
		if input.Report == nil || input.Report.State != "unknown" {
			t.Fatalf("%+v", input.Report)
		}
		cancel()
		return response(p.SyncResponse{Version: p.Version}), nil
	})}
	if err := a.Run(ctx); err != nil {
		t.Fatal(err)
	}
}
