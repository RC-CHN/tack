package control

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	p "tack.local/management/protocol"
)

func v3Sync(id string) p.SyncRequest { in := v2Sync(id); in.Version = p.RetentionVersion; return in }

func TestReceiptRequiredBeforeEvictionAndRestart(t *testing.T) {
	s, c, path := fixture(t)
	s.config.MaxHistoryTasks = 2
	var protected p.Task
	for i := 0; i < 30; i++ {
		task, chunk := pendingResult(t, s, c, &p.Result{Stdout: bytes.Repeat([]byte("x"), 100)})
		chunk.Data = bytes.Repeat([]byte("x"), 100)
		if call(t, s, "POST", p.NodeResultPath, c.Nodes[0].Token, chunk, nil) != 200 {
			t.Fatal("upload")
		}
		if i == 0 {
			protected = task
			continue
		}
		in := v3Sync("node-a")
		in.Receipts = []string{task.ID}
		var out p.SyncResponse
		if call(t, s, "POST", p.NodeRetentionPath, c.Nodes[0].Token, in, &out) != 200 || len(out.Receipts) != 1 {
			t.Fatal("receipt")
		}
		if s.historyTasks > 2 {
			t.Fatal("unbounded history")
		}
	}
	if _, ok, err := s.lookupTask(protected.ID); err != nil || !ok {
		t.Fatal("unconfirmed output was evicted")
	}
	// Age every eligible file, and restart with a tiny retention interval.
	entries, _ := os.ReadDir(filepath.Dir(s.releasedPath("unused")))
	for _, entry := range entries {
		old := time.Now().Add(-time.Hour)
		os.Chtimes(filepath.Join(filepath.Dir(s.releasedPath("unused")), entry.Name()), old, old)
	}
	c.HistoryRetentionSeconds = 1
	s, err := New(c, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Maintain(); err != nil {
		t.Fatal(err)
	}
	if s.historyTasks != 1 {
		t.Fatalf("expired confirmed history retained: %d", s.historyTasks)
	}
	// The final ack was lost: a probe must still retrieve it after churn/restart.
	in := v3Sync("node-a")
	call(t, s, "POST", p.NodeRetentionPath, c.Nodes[0].Token, in, nil)
	manifest := p.OutputManifest(&p.Result{Stdout: bytes.Repeat([]byte("x"), 100)})
	probe := p.ResultChunk{Version: p.TransferVersion, NodeID: "node-a", InstanceID: in.InstanceID, TaskID: protected.ID, SHA256: manifest.SHA256, Probe: true}
	var ack p.ChunkAck
	if call(t, s, "POST", p.NodeResultPath, c.Nodes[0].Token, probe, &ack) != 200 || !ack.Complete {
		t.Fatal("lost final ack could not be recovered")
	}
}

func TestReplayFloorSurvivesRestartAndNeverDecreases(t *testing.T) {
	s, c, path := fixture(t)
	in := v3Sync("node-a")
	in.ReplayHigh = 50000
	if call(t, s, "POST", p.NodeRetentionPath, c.Nodes[0].Token, in, nil) != 200 {
		t.Fatal("watermark")
	}
	s, err := New(c, path)
	if err != nil {
		t.Fatal(err)
	}
	in.ReplayHigh = 3
	call(t, s, "POST", p.NodeRetentionPath, c.Nodes[0].Token, in, nil)
	var task p.Task
	if call(t, s, "POST", "/v1/tasks", c.AdminToken, taskInput(), &task) != 201 || task.Sequence != 50001 {
		t.Fatalf("sequence regressed: %+v", task)
	}
	in.ReplayInit = true
	in.AcceptTask = false
	var out p.SyncResponse
	if call(t, s, "POST", p.NodeRetentionPath, c.Nodes[0].Token, in, &out) != 200 || out.ReplayFloor == nil || *out.ReplayFloor != 50001 || out.Task != nil {
		t.Fatal("invalid migration fence")
	}
}

func TestCompletedArchiveWinsOverRestoredActiveSnapshot(t *testing.T) {
	s, c, path := fixture(t)
	task, chunk := pendingResult(t, s, c, &p.Result{Stdout: []byte("keep original")})
	old, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	chunk.Data = []byte("keep original")
	if call(t, s, "POST", p.NodeResultPath, c.Nodes[0].Token, chunk, nil) != 200 {
		t.Fatal("upload")
	}
	if err := p.WriteFile(path, old); err != nil {
		t.Fatal(err)
	}
	s, err = New(c, path)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.state.Tasks) != 0 {
		t.Fatal("completed task was resurrected")
	}
	saved, ok, err := s.lookupTask(task.ID)
	if err != nil || !ok || string(saved.Result.Stdout) != "keep original" {
		t.Fatal("durable output replaced by stale snapshot")
	}
}

func TestEvictionCacheAmortizesScansAndOrdersLateReceipts(t *testing.T) {
	s, _, _ := fixture(t)
	s.config.MaxHistoryTasks = 300
	for i := 0; i < 300; i++ {
		if err := s.archive(p.Task{ID: fmt.Sprintf("old-%03d", i), NodeID: "node-a", State: "canceled"}); err != nil {
			t.Fatal(err)
		}
	}
	before := s.evictions.scans
	for i := 0; i < 270; i++ {
		if err := s.pruneHistory(&p.Task{}); err != nil {
			t.Fatal(err)
		}
		if s.historyTasks != 299 || len(s.evictions.items) > evictionBatch {
			t.Fatal("unbounded or incorrect eviction")
		}
		if err := s.archive(p.Task{ID: fmt.Sprintf("new-%03d", i), NodeID: "node-a", State: "canceled"}); err != nil {
			t.Fatal(err)
		}
		if i == 40 {
			// A long-delayed receipt must enter ahead of the remaining batch,
			// even when later files are waiting outside the bounded cache.
			id := "late-receipt"
			if err := s.archive(p.Task{ID: id, NodeID: "node-a", State: "succeeded", Result: &p.Result{}}); err != nil {
				t.Fatal(err)
			}
			old := time.Now().Add(-time.Hour)
			if err := os.Chtimes(s.historyPath(id), old, old); err != nil {
				t.Fatal(err)
			}
			if err := s.releaseReceipts("node-a", []string{id}); err != nil {
				t.Fatal(err)
			}
			if err := s.pruneHistory(nil); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(s.releasedPath(id)); !os.IsNotExist(err) {
				t.Fatal("late old result did not evict first", err)
			}
		}
	}
	if scans := s.evictions.scans - before; scans > 3 {
		t.Fatalf("270 admissions required %d directory scans", scans)
	}
	for i := 0; i < 300; i++ {
		_, err := os.Stat(s.releasedPath(fmt.Sprintf("old-%03d", i)))
		if (i < 270) != os.IsNotExist(err) {
			t.Fatalf("oldest-first order broken at %d: %v", i, err)
		}
	}
	// Periodic maintenance refreshes metadata, including filesystem changes.
	old := time.Now().Add(-31 * 24 * time.Hour)
	if err := os.Chtimes(s.releasedPath("new-269"), old, old); err != nil {
		t.Fatal(err)
	}
	if err := s.Maintain(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.releasedPath("new-269")); !os.IsNotExist(err) {
		t.Fatal("idle TTL cleanup failed", err)
	}
}

func TestReceiptOwnerHeaderValidation(t *testing.T) {
	s, _, _ := fixture(t)
	for _, header := range []string{
		`{"id":"different","node_id":"node-a"}`,
		`{"id":"header-test","sequence":-1,"node_id":"node-a"}`,
		`{"id":"header-test","node_id":"../bad"}`,
		`{"id":"header-test","command":"unexpected"}`,
	} {
		if err := os.WriteFile(s.historyPath("header-test"), []byte(header), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := s.receiptOwner("header-test"); err == nil {
			t.Fatalf("accepted invalid header: %s", header)
		}
	}
	if err := s.archive(p.Task{ID: "header-test", Sequence: 42, NodeID: "node-a", State: "succeeded", Result: &p.Result{}}); err != nil {
		t.Fatal(err)
	}
	if err := s.releaseReceipts("wrong-node", []string{"header-test"}); err == nil {
		t.Fatal("accepted another node's receipt")
	}
	if _, err := os.Stat(s.historyPath("header-test")); err != nil {
		t.Fatal("unauthorized receipt moved result", err)
	}
}

func TestBytePressureReclaimsOnlyConfirmedOutput(t *testing.T) {
	s, _, _ := fixture(t)
	s.config.MaxHistoryBytes = 220000 // Exercise bytes while count is far below its limit.
	output := bytes.Repeat([]byte("x"), 50000)
	if err := s.archive(p.Task{ID: "pinned", NodeID: "node-a", State: "succeeded", Result: &p.Result{Stdout: output}}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 30; i++ {
		id := fmt.Sprintf("byte-%d", i)
		if err := s.pruneHistory(&p.Task{}); err != nil {
			t.Fatal(err)
		}
		if s.historyBytes+reservation(p.Task{}) > s.config.MaxHistoryBytes {
			t.Fatal("byte budget not reclaimed")
		}
		if err := s.archive(p.Task{ID: id, NodeID: "node-a", State: "succeeded", Result: &p.Result{Stdout: output}}); err != nil {
			t.Fatal(err)
		}
		if err := s.releaseReceipts("node-a", []string{id}); err != nil {
			t.Fatal(err)
		}
	}
	if s.historyTasks > 4 {
		t.Fatal("bytes did not bound history", s.historyTasks)
	}
	if _, err := os.Stat(s.historyPath("pinned")); err != nil {
		t.Fatal("unconfirmed output evicted", err)
	}
}

func TestStartupPreservesAndAccountsUnknownUploads(t *testing.T) {
	_, c, path := fixture(t)
	dir := path + ".uploads"
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(dir, "unresolved.part")
	if err := os.WriteFile(orphan, []byte("protect"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".state-123", "abandoned.json.tack-pending"} {
		if err := os.WriteFile(filepath.Join(path+".history", name), []byte("incomplete"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	s, err := New(c, path)
	if err != nil {
		t.Fatal(err)
	}
	if s.orphanFiles != 1 || s.orphanBytes < 4096 {
		t.Fatal("unresolved staging excluded from capacity")
	}
	b, err := os.ReadFile(orphan)
	if err != nil || string(b) != "protect" {
		t.Fatal("unresolved staging was discarded", err)
	}
	for _, name := range []string{".state-123", "abandoned.json.tack-pending"} {
		if _, err := os.Stat(filepath.Join(path+".history", name)); !os.IsNotExist(err) {
			t.Fatal("known temporary file retained", err)
		}
	}
}

func TestLegacyUndispatchedCancellationBecomesReclaimable(t *testing.T) {
	for _, inSnapshot := range []bool{false, true} {
		_, c, path := fixture(t)
		old := p.Task{ID: "never-dispatched", NodeID: "node-a", State: "canceled"}
		if err := p.WriteJSON(filepath.Join(path+".history", old.ID+".json"), old); err != nil {
			t.Fatal(err)
		}
		tasks := map[string]p.Task{}
		if inSnapshot {
			tasks[old.ID] = old
		}
		if err := p.WriteJSON(path, state{Version: 1, Tasks: tasks}); err != nil {
			t.Fatal(err)
		}
		s, err := New(c, path)
		if err != nil {
			t.Fatal(err)
		}
		if s.historyTasks != 1 {
			t.Fatal("duplicate archive during migration", s.historyTasks)
		}
		if _, err := os.Stat(s.releasedPath(old.ID)); err != nil {
			t.Fatal("old cancellation pinned forever", err)
		}
		if _, err := os.Stat(s.historyPath(old.ID)); !os.IsNotExist(err) {
			t.Fatal("duplicate old archive", err)
		}
		s.config.MaxHistoryTasks = 1
		var task p.Task
		if call(t, s, "POST", "/v1/tasks", c.AdminToken, taskInput(), &task) != 201 {
			t.Fatal("migrated history blocked admission")
		}
	}
}
