package node

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	p "tack.local/management/protocol"
)

func TestRecoverUnconfirmedResultBeforeCleaningTemps(t *testing.T) {
	cfg := testConfig(t)
	a, _ := New(cfg)
	running := journal{Task: p.Task{ID: "saved", NodeID: cfg.NodeID, Sequence: 12}, Report: p.Report{TaskID: "saved", State: "running"}}
	finished := running
	finished.Report.State = "succeeded"
	finished.Report.Result = &p.Result{Stdout: []byte("must survive")}
	path := filepath.Join(cfg.StateDir, "journal.json")
	if err := p.WriteJSON(path, retainedJournal{StorageVersion: 3, Records: map[string]journal{"saved": running}}); err != nil {
		t.Fatal(err)
	}
	if err := p.WriteJSON(path+".tack-pending", retainedJournal{StorageVersion: 3, Records: map[string]journal{"saved": finished}}); err != nil {
		t.Fatal(err)
	}
	if err := p.WriteJSON(a.replayPath()+".tack-pending", replayCheckpoint{Version: 1, NodeID: cfg.NodeID, High: 12}); err != nil {
		t.Fatal(err)
	}
	if err := a.recoverReplay(); err != nil {
		t.Fatal(err)
	}
	records, format, err := readJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	write := a.writeJSON
	a.writeJSON = func(string, any) error { return syscall.ENOSPC }
	if _, _, err := a.recoverJournal(records, format); err == nil {
		t.Fatal("expected disk failure")
	}
	if _, err := os.Stat(path + ".tack-pending"); err != nil {
		t.Fatal("lost recovery source on failed persistence")
	}
	a.writeJSON = write
	records, format, err = a.recoverJournal(records, format)
	if err != nil || !format || string(records["saved"].Report.Result.Stdout) != "must survive" {
		t.Fatal("recovery discarded unconfirmed output", err)
	}
	if _, err := os.Stat(path + ".tack-pending"); !os.IsNotExist(err) {
		t.Fatal("committed recovery source retained")
	}
}

func TestResultPersistenceRetriesWithoutReexecution(t *testing.T) {
	cfg := testConfig(t)
	exe, _ := os.Executable()
	marker := filepath.Join(t.TempDir(), "executions")
	task := p.Task{ID: "disk-retry", NodeID: cfg.NodeID, Sequence: 1, State: "dispatched", Command: p.Command{Mode: "exec", Program: exe, Args: []string{"-test.run=^TestHelperExecution$"}, Env: map[string]string{"TACK_TEST_HELPER": "1", "TACK_TEST_MARKER": marker, "GORACE": "atexit_sleep_ms=0"}, TimeoutSeconds: 5, OutputLimit: 100}}
	failures := 0
	runRetainedTask(t, cfg, task, "succeeded", 0, func(a *Node) {
		write := a.writeJSON
		a.writeJSON = func(path string, value any) error {
			if j, ok := value.(retainedJournal); ok {
				if rec, ok := j.Records[task.ID]; ok && rec.Report.State == "succeeded" && !rec.Acknowledged && failures < 2 {
					failures++
					return syscall.ENOSPC
				}
			}
			return write(path, value)
		}
	})
	b, err := os.ReadFile(marker)
	if err != nil || string(b) != "x" || failures != 2 {
		t.Fatalf("executions=%q failures=%d err=%v", b, failures, err)
	}
}

func TestRecoverySourcesReadBeforeBatchedLegacyCleanup(t *testing.T) {
	cfg := testConfig(t)
	a, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 260; i++ {
		if err := os.WriteFile(filepath.Join(cfg.StateDir, fmt.Sprintf(".state-%d", i)), []byte("{}"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(cfg.StateDir, "journal.json")
	for i, suffix := range []string{".recovery", ".recovery.tack-pending", ".tack-pending"} {
		id := fmt.Sprintf("result-%d", i)
		rec := journal{Task: p.Task{ID: id, NodeID: cfg.NodeID}, Report: p.Report{TaskID: id, State: "succeeded", Result: &p.Result{Stdout: []byte(id)}}}
		if err := p.WriteJSON(path+suffix, map[string]journal{id: rec}); err != nil {
			t.Fatal(err)
		}
	}
	// Every pending source must be merged before the very first recovery write.
	write := a.writeJSON
	a.writeJSON = func(path string, value any) error {
		recs, ok := value.(map[string]journal)
		if !ok || len(recs) != 3 {
			t.Fatalf("overwriting recovery source before reading all results: %T", value)
		}
		return write(path, value)
	}
	records, _, err := a.recoverJournal(map[string]journal{}, false)
	if err != nil || len(records) != 3 {
		t.Fatal("lost complete recovery output", err)
	}
	for id, rec := range records {
		if string(rec.Report.Result.Stdout) != id {
			t.Fatal("changed recovery output")
		}
	}
	entries, err := os.ReadDir(cfg.StateDir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "journal.json" {
		t.Fatal("temporary files not reclaimed", entries, err)
	}
}
