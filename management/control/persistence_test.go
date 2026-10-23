package control

import (
	"os"
	"path/filepath"
	"testing"

	p "tack.local/management/protocol"
)

// A path fault is deterministic even under root. It simulates a failed durable
// replacement without filling the developer's filesystem or changing mounts.
func breakStateDestination(t *testing.T, path string) func() {
	t.Helper()
	saved := path + ".saved"
	if err := os.Rename(path, saved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	return func() {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(saved, path); err != nil {
			t.Fatal(err)
		}
	}
}
func TestFailedCommitDoesNotAcknowledgeOrLeakTemporaryState(t *testing.T) {
	s, c, path := fixture(t)
	restore := breakStateDestination(t, path)
	if code := call(t, s, "POST", "/v1/tasks", c.AdminToken, taskInput(), nil); code != 500 {
		t.Fatal(code)
	}
	if len(s.state.Tasks) != 0 {
		t.Fatal("failed commit stayed in memory")
	}
	files, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".state-*"))
	if len(files) != 0 {
		t.Fatal("state temporary files leaked")
	}
	restore()
	if code := call(t, s, "POST", "/v1/tasks", c.AdminToken, taskInput(), nil); code != 201 {
		t.Fatal("did not recover after storage repair")
	}
}
func TestFinalUploadAckWaitsForDurableState(t *testing.T) {
	s, c, path := fixture(t)
	task, chunk := pendingResult(t, s, c, &p.Result{Stdout: []byte("durable output")})
	restore := breakStateDestination(t, path)
	chunk.Data = []byte("durable output")
	if code := call(t, s, "POST", p.NodeResultPath, c.Nodes[0].Token, chunk, nil); code != 500 {
		t.Fatal(code)
	}
	if !s.state.Tasks[task.ID].ResultPending || len(s.state.Tasks[task.ID].Result.Stdout) != 0 {
		t.Fatal("failed final commit acknowledged in memory")
	}
	restore()
	// A complete archive may already exist while the active-state replacement
	// failed. Restart must prefer the pending state and reconcile without replay.
	var err error
	s, err = New(c, path)
	if err != nil {
		t.Fatal(err)
	}
	call(t, s, "POST", p.NodeControlPath, c.Nodes[0].Token, v2Sync("node-a"), nil)
	chunk.Probe, chunk.Data = true, nil
	var ack p.ChunkAck
	if code := call(t, s, "POST", p.NodeResultPath, c.Nodes[0].Token, chunk, &ack); code != 200 || !ack.Complete {
		t.Fatalf("durable offset did not recover: %d %+v", code, ack)
	}
	files, _ := filepath.Glob(filepath.Join(path+".uploads", "*.part"))
	if len(files) != 0 {
		t.Fatal("complete upload retained its staging file")
	}
	if s.historyTasks != 1 {
		t.Fatal("recovered commit duplicated history")
	}
}
