package node

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	p "tack.local/management/protocol"
)

func runRetainedTask(t *testing.T, cfg p.NodeConfig, task p.Task, wantState string, fence uint64, configure ...func(*Node)) {
	t.Helper()
	a, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	a.Logf = nil
	for _, setup := range configure {
		setup(a)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	seen, dispatched, receipt := false, false, false
	a.client = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/health" {
			return response(map[string]int{"protocol_version": p.LatestVersion}), nil
		}
		var in p.SyncRequest
		if err := p.Decode(r.Body, &in); err != nil {
			return nil, err
		}
		out := p.SyncResponse{Version: p.LatestVersion, Receipts: in.Receipts}
		if in.ReplayInit {
			out.ReplayFloor = &fence
			return response(out), nil
		}
		if in.Report != nil && p.Terminal(in.Report.State) {
			if in.Report.State != wantState {
				t.Errorf("state=%s want=%s", in.Report.State, wantState)
			}
			seen = true
			out.Acknowledged = in.Report.TaskID
		}
		for _, id := range in.Receipts {
			if id == task.ID {
				receipt = true
			}
		}
		if receipt && in.Node.Metrics.JournalRecords == 0 {
			cancel()
			return response(out), nil
		}
		if !dispatched && in.AcceptTask {
			out.Task = &task
			dispatched = true
		}
		return response(out), nil
	})}
	a.uploadClient = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		var in p.ResultChunk
		if err := p.Decode(r.Body, &in); err != nil {
			return nil, err
		}
		return response(p.ChunkAck{Version: p.TransferVersion, TaskID: in.TaskID, Complete: true}), nil
	})}
	if err := a.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if !seen || !receipt {
		t.Fatal("task was not finalized and receipt-confirmed")
	}
	records, retained, err := readJournal(filepath.Join(cfg.StateDir, "journal.json"))
	if err != nil || !retained || len(records) != 0 {
		t.Fatalf("journal did not compact: %d %v", len(records), err)
	}
}

func TestCompactedReplayWatermarkAcrossRestart(t *testing.T) {
	cfg := testConfig(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "executions")
	task := p.Task{ID: "first", NodeID: cfg.NodeID, Sequence: 1, State: "dispatched", Command: p.Command{Mode: "exec", Program: exe, Args: []string{"-test.run=^TestHelperExecution$"}, Env: map[string]string{"TACK_TEST_HELPER": "1", "TACK_TEST_MARKER": marker, "GORACE": "atexit_sleep_ms=0"}, TimeoutSeconds: 5, OutputLimit: 100}}
	runRetainedTask(t, cfg, task, "succeeded", 0)
	runRetainedTask(t, cfg, task, "unknown", 0)
	task.ID, task.Sequence = "second", 2
	runRetainedTask(t, cfg, task, "succeeded", 0)
	// Crash after checkpoint but before intent: uncertain work must not execute.
	if err := p.WriteJSON(filepath.Join(cfg.StateDir, "replay.json"), replayCheckpoint{Version: 1, NodeID: cfg.NodeID, High: 3}); err != nil {
		t.Fatal(err)
	}
	task.ID, task.Sequence = "fenced-before-intent", 3
	runRetainedTask(t, cfg, task, "unknown", 0)
	b, err := os.ReadFile(marker)
	if err != nil || string(b) != "xx" {
		t.Fatalf("unexpected executions %q: %v", b, err)
	}
	var old map[string]journal
	if err := p.ReadJSON(filepath.Join(cfg.StateDir, "journal.json"), &old); err == nil {
		t.Fatal("old binary can read compacted state")
	}
	a, _ := New(cfg)
	a.version = p.TransferVersion
	if _, err := a.openReplay(context.Background(), nil); err == nil {
		t.Fatal("unsafe protocol downgrade allowed")
	}
}

func TestFullLegacyJournalMigratesAndKeepsAccepting(t *testing.T) {
	cfg := testConfig(t)
	records := map[string]journal{}
	for i := 0; i < 10000; i++ {
		id := fmt.Sprintf("legacy-%05d", i)
		records[id] = acknowledge(journal{Task: p.Task{ID: id, NodeID: cfg.NodeID}, Report: p.Report{TaskID: id, State: "succeeded", Result: &p.Result{}}})
	}
	if err := p.WriteJSON(filepath.Join(cfg.StateDir, "journal.json"), records); err != nil {
		t.Fatal(err)
	}
	exe, _ := os.Executable()
	marker := filepath.Join(t.TempDir(), "executions")
	task := p.Task{ID: "after-full-legacy", NodeID: cfg.NodeID, Sequence: 10001, State: "dispatched", Command: p.Command{Mode: "exec", Program: exe, Args: []string{"-test.run=^TestHelperExecution$"}, Env: map[string]string{"TACK_TEST_HELPER": "1", "TACK_TEST_MARKER": marker, "GORACE": "atexit_sleep_ms=0"}, TimeoutSeconds: 5, OutputLimit: 100}}
	writes := 0
	runRetainedTask(t, cfg, task, "succeeded", 10000, func(a *Node) {
		write := a.writeJSON
		a.writeJSON = func(path string, value any) error {
			if _, ok := value.(retainedJournal); ok {
				writes++
			}
			return write(path, value)
		}
	})
	if writes > 15 {
		t.Fatalf("legacy receipt migration rewrote the full journal %d times", writes)
	}
	task.ID, task.Sequence = "legacy-00000", 0
	runRetainedTask(t, cfg, task, "unknown", 0)
	b, err := os.ReadFile(marker)
	if err != nil || string(b) != "x" {
		t.Fatal("legacy replay executed")
	}
}
