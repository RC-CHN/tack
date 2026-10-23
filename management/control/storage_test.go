package control

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"

	p "tack.local/management/protocol"
)

func storageHeap() uint64 {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

func TestCompletedHistoryIsNotRetainedAndPagesAreBounded(t *testing.T) {
	s, c, path := fixture(t)
	var first string
	var baseline uint64
	for i := 0; i < 220; i++ {
		var task p.Task
		call(t, s, "POST", "/v1/tasks", c.AdminToken, taskInput(), &task)
		if i == 0 {
			first = task.ID
		}
		in := syncInput("node-a")
		call(t, s, "POST", p.NodeSyncPath, c.Nodes[0].Token, in, nil)
		in.Report = &p.Report{TaskID: task.ID, State: "succeeded", Result: &p.Result{Stdout: bytes.Repeat([]byte("o"), 32768)}}
		var ack p.SyncResponse
		if code := call(t, s, "POST", p.NodeSyncPath, c.Nodes[0].Token, in, &ack); code != 200 || ack.Acknowledged != task.ID {
			t.Fatalf("report %d: %d %+v", i, code, ack)
		}
		if len(s.state.Tasks) != 0 {
			t.Fatal("completed task retained in active memory")
		}
		if i == 19 {
			baseline = storageHeap()
		}
	}
	retained := storageHeap()
	if int64(retained)-int64(baseline) > 2*1024*1024 {
		t.Fatalf("history retained heap: %d -> %d", baseline, retained)
	}
	if s.historyBytes < 7*1024*1024 || s.historyTasks != 220 {
		t.Fatal("history not persisted")
	}
	info, _ := os.Stat(path)
	if info.Size() > 256 {
		t.Fatal("active snapshot grew with history")
	}
	var original p.Task
	if call(t, s, "GET", "/v1/tasks/"+first, c.AdminToken, nil, &original) != 200 || len(original.Result.Stdout) != 32768 {
		t.Fatal("historical result missing")
	}
	seen, cursor := map[string]bool{}, ""
	for {
		var page p.TaskPage
		if call(t, s, "GET", "/v2/tasks?limit=13&after="+cursor, c.AdminToken, nil, &page) != 200 || len(page.Tasks) > 13 {
			t.Fatal("unbounded task page")
		}
		for _, task := range page.Tasks {
			if seen[task.ID] || task.ID <= cursor || task.Result != nil || task.Command.Program != "" {
				t.Fatal("invalid page or retained payload")
			}
			seen[task.ID] = true
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if len(seen) != 220 {
		t.Fatalf("pagination lost history: %d", len(seen))
	}
	restarted, err := New(c, path)
	if err != nil {
		t.Fatal(err)
	}
	if len(restarted.state.Tasks) != 0 || restarted.historyTasks != 220 {
		t.Fatal("restart loaded historical payloads")
	}
	in := syncInput("node-a")
	in.Report = &p.Report{TaskID: first, State: "succeeded", Result: original.Result}
	var ack p.SyncResponse
	if call(t, restarted, "POST", p.NodeSyncPath, c.Nodes[0].Token, in, &ack) != 200 || ack.Acknowledged != first {
		t.Fatal("archival broke lost acknowledgement recovery")
	}
	t.Logf("220 archived tasks, history=%d bytes, active=%d bytes, live heap=%d -> %d", s.historyBytes, info.Size(), baseline, retained)
}

func TestStorageLimitsRejectBeforeAcceptance(t *testing.T) {
	s, c, _ := fixture(t)
	s.config.MaxActiveTasks = 1
	var task p.Task
	if call(t, s, "POST", "/v1/tasks", c.AdminToken, taskInput(), &task) != 201 {
		t.Fatal("first task")
	}
	if call(t, s, "POST", "/v1/tasks", c.AdminToken, taskInput(), nil) != 503 {
		t.Fatal("active cap ignored")
	}
	call(t, s, "POST", "/v1/tasks/"+task.ID+"/cancel", c.AdminToken, nil, nil)
	s.config.MaxHistoryTasks = 1
	if call(t, s, "POST", "/v1/tasks", c.AdminToken, taskInput(), nil) != 503 {
		t.Fatal("history cap ignored")
	}
	s.config.MaxHistoryTasks = 100
	s.config.MaxHistoryBytes = s.historyBytes + 1
	if call(t, s, "POST", "/v1/tasks", c.AdminToken, taskInput(), nil) != 503 {
		t.Fatal("result capacity was not reserved")
	}
	for _, query := range []string{"limit=0", "limit=201", "after=../escape"} {
		if call(t, s, "GET", "/v2/tasks?"+query, c.AdminToken, nil, nil) != 400 {
			t.Fatal(query)
		}
	}
}

func TestLegacyMigrationAndCompletedUploadCleanup(t *testing.T) {
	_, c, path := fixture(t)
	done := p.Task{ID: "done", NodeID: "node-a", State: "succeeded", Result: &p.Result{Stdout: []byte("keep")}}
	manifest := p.OutputManifest(done.Result)
	done.Upload = &manifest
	pending := done
	pending.ID = "pending"
	pending.ResultPending = true
	legacy := map[string]any{"tasks": map[string]p.Task{done.ID: done, pending.ID: pending}}
	if err := p.WriteJSON(path, legacy); err != nil {
		t.Fatal(err)
	}
	dir := path + ".uploads"
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"done", "pending"} {
		if err := os.WriteFile(filepath.Join(dir, id+"."+manifest.SHA256+".part"), []byte("keep"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	s, err := New(c, path)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.state.Tasks) != 1 || !s.state.Tasks["pending"].ResultPending {
		t.Fatal("migration lost pending output")
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.part"))
	if len(files) != 1 || !strings.Contains(files[0], "pending.") {
		t.Fatal("incorrect staging cleanup")
	}
	for i := 0; i < 2; i++ {
		s, err = New(c, path)
		if err != nil {
			t.Fatal(err)
		}
		task, ok, err := s.lookupTask("done")
		if err != nil || !ok || string(task.Result.Stdout) != "keep" {
			t.Fatal("migration not restart safe")
		}
	}
}

func TestHistoryDiskFullDoesNotAcknowledge(t *testing.T) {
	s, c, _ := fixture(t)
	var task p.Task
	call(t, s, "POST", "/v1/tasks", c.AdminToken, taskInput(), &task)
	in := syncInput("node-a")
	call(t, s, "POST", p.NodeSyncPath, c.Nodes[0].Token, in, nil)
	write := s.writeJSON
	s.writeJSON = func(path string, v any) error {
		if strings.Contains(path, ".history") {
			return syscall.ENOSPC
		}
		return write(path, v)
	}
	in.Report = &p.Report{TaskID: task.ID, State: "succeeded", Result: &p.Result{Stdout: []byte("retry")}}
	if call(t, s, "POST", p.NodeSyncPath, c.Nodes[0].Token, in, nil) != 500 {
		t.Fatal("disk full acknowledged")
	}
	if s.state.Tasks[task.ID].State != "dispatched" {
		t.Fatal("failed archive mutated queue")
	}
	s.writeJSON = write
	var ack p.SyncResponse
	if call(t, s, "POST", p.NodeSyncPath, c.Nodes[0].Token, in, &ack) != 200 || ack.Acknowledged != task.ID {
		t.Fatal("did not recover")
	}
}

func TestSlowStateWriteDoesNotBlockReaders(t *testing.T) {
	s, c, _ := fixture(t)
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	write := s.writeJSON
	s.writeJSON = func(path string, v any) error { close(started); <-release; return write(path, v) }
	defer func() { close(release); <-done }()
	body, _ := json.Marshal(taskInput())
	r := httptest.NewRequest("POST", "/v1/tasks", bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+c.AdminToken)
	go func() { defer close(done); s.AdminHandler().ServeHTTP(httptest.NewRecorder(), r) }()
	<-started
	concurrentAdmin(t, s, c.AdminToken, "/v1/metrics")
	concurrentAdmin(t, s, c.AdminToken, "/v1/tasks")
}

func TestConcurrentCompletionAndCancellationNeverResurrectsTask(t *testing.T) {
	s, c, _ := fixture(t)
	for i := 0; i < 40; i++ {
		var task p.Task
		call(t, s, "POST", "/v1/tasks", c.AdminToken, taskInput(), &task)
		input := syncInput("node-a")
		call(t, s, "POST", p.NodeSyncPath, c.Nodes[0].Token, input, nil)
		input.Report = &p.Report{TaskID: task.ID, State: "succeeded", Result: &p.Result{Stdout: []byte("done")}}
		start := make(chan struct{})
		var workers sync.WaitGroup
		workers.Add(2)
		go func() {
			defer workers.Done()
			<-start
			call(t, s, "POST", p.NodeSyncPath, c.Nodes[0].Token, input, nil)
		}()
		go func() {
			defer workers.Done()
			<-start
			call(t, s, "POST", "/v1/tasks/"+task.ID+"/cancel", c.AdminToken, nil, nil)
		}()
		close(start)
		workers.Wait()
		final, ok, err := s.lookupTask(task.ID)
		if err != nil || !ok || final.State != "succeeded" || len(s.state.Tasks) != 0 {
			t.Fatalf("completed task resurrected: %+v err=%v", final, err)
		}
	}
}
