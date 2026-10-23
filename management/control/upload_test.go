package control

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	p "tack.local/management/protocol"
)

func v2Sync(id string) p.SyncRequest {
	in := syncInput(id)
	in.Version, in.AcceptTask, in.LeaseSeconds = p.LatestVersion, true, 9000
	return in
}
func pendingResult(t *testing.T, s *Server, c p.ControlConfig, result *p.Result) (p.Task, p.ResultChunk) {
	t.Helper()
	var task p.Task
	in := taskInput()
	in.Command.OutputLimit = p.MaxOutput
	if code := call(t, s, "POST", "/v1/tasks", c.AdminToken, in, &task); code != 201 {
		t.Fatal(code)
	}
	sync := v2Sync("node-a")
	if code := call(t, s, "POST", p.NodeControlPath, c.Nodes[0].Token, sync, nil); code != 200 {
		t.Fatal(code)
	}
	manifest := p.OutputManifest(result)
	metadata := *result
	metadata.Stdout, metadata.Stderr = nil, nil
	sync.Report = &p.Report{TaskID: task.ID, State: "succeeded", Result: &metadata, Upload: &manifest}
	var ack p.SyncResponse
	if code := call(t, s, "POST", p.NodeControlPath, c.Nodes[0].Token, sync, &ack); code != 200 || ack.Acknowledged != task.ID {
		t.Fatalf("metadata HTTP %d: %+v", code, ack)
	}
	call(t, s, "GET", "/v1/tasks/"+task.ID, c.AdminToken, nil, &task)
	if !task.ResultPending {
		t.Fatal("result was marked complete before output arrived")
	}
	return task, p.ResultChunk{Version: p.LatestVersion, NodeID: "node-a", InstanceID: sync.InstanceID, TaskID: task.ID, SHA256: manifest.SHA256}
}
func TestUploadResumeAfterControllerRestartAndLostAck(t *testing.T) {
	s, c, path := fixture(t)
	want := &p.Result{Stdout: bytes.Repeat([]byte("a"), p.MaxChunk+13), Stderr: []byte("stderr")}
	task, chunk := pendingResult(t, s, c, want)
	var ack p.ChunkAck
	chunk.Data = want.Stdout[:p.MaxChunk]
	if code := call(t, s, "POST", p.NodeResultPath, c.Nodes[0].Token, chunk, &ack); code != 200 || ack.Offset != p.MaxChunk || ack.Complete {
		t.Fatalf("%d %+v", code, ack)
	}
	// The sender loses this acknowledgement and retries exactly the same bytes.
	if code := call(t, s, "POST", p.NodeResultPath, c.Nodes[0].Token, chunk, &ack); code != 200 || ack.Offset != p.MaxChunk {
		t.Fatalf("duplicate chunk: %d %+v", code, ack)
	}
	chunk.Data = []byte("different")
	if code := call(t, s, "POST", p.NodeResultPath, c.Nodes[0].Token, chunk, nil); code != 409 {
		t.Fatal(code)
	}
	var err error
	s, err = New(c, path)
	if err != nil {
		t.Fatal(err)
	}
	call(t, s, "POST", p.NodeControlPath, c.Nodes[0].Token, v2Sync("node-a"), nil)
	chunk.Probe, chunk.Data = true, nil
	if code := call(t, s, "POST", p.NodeResultPath, c.Nodes[0].Token, chunk, &ack); code != 200 || ack.Offset != p.MaxChunk || ack.Complete {
		t.Fatalf("restart probe: %d %+v", code, ack)
	}
	chunk.Probe, chunk.Offset, chunk.Data = false, ack.Offset, append(append([]byte{}, want.Stdout[p.MaxChunk:]...), want.Stderr...)
	if code := call(t, s, "POST", p.NodeResultPath, c.Nodes[0].Token, chunk, &ack); code != 200 || !ack.Complete {
		t.Fatalf("completion: %d %+v", code, ack)
	}
	call(t, s, "GET", "/v1/tasks/"+task.ID, c.AdminToken, nil, &task)
	if task.ResultPending || !bytes.Equal(task.Result.Stdout, want.Stdout) || !bytes.Equal(task.Result.Stderr, want.Stderr) || task.Timing.ResultReceivedAt.IsZero() {
		t.Fatal("incorrect final output/timing")
	}
	// A lost final acknowledgement is also recoverable after another restart.
	s, err = New(c, path)
	if err != nil {
		t.Fatal(err)
	}
	call(t, s, "POST", p.NodeControlPath, c.Nodes[0].Token, v2Sync("node-a"), nil)
	chunk.Probe, chunk.Data = true, nil
	if code := call(t, s, "POST", p.NodeResultPath, c.Nodes[0].Token, chunk, &ack); code != 200 || !ack.Complete {
		t.Fatalf("final ack recovery: %d %+v", code, ack)
	}
}
func TestUploadAuthorizationLimitsAndDigest(t *testing.T) {
	s, c, _ := fixture(t)
	_, chunk := pendingResult(t, s, c, &p.Result{Stdout: []byte("abcd")})
	chunk.Data = []byte("abcd")
	if code := call(t, s, "POST", p.NodeResultPath, c.AdminToken, chunk, nil); code != 401 {
		t.Fatal(code)
	}
	bad := chunk
	bad.NodeID, bad.InstanceID = "node-b", "instance-node-b"
	if code := call(t, s, "POST", p.NodeResultPath, c.Nodes[1].Token, bad, nil); code != 403 {
		t.Fatal(code)
	}
	bad = chunk
	bad.InstanceID = "different"
	if code := call(t, s, "POST", p.NodeResultPath, c.Nodes[0].Token, bad, nil); code != 409 {
		t.Fatal(code)
	}
	bad = chunk
	bad.Offset, bad.Data = 1, []byte("b")
	if code := call(t, s, "POST", p.NodeResultPath, c.Nodes[0].Token, bad, nil); code != 409 {
		t.Fatal(code)
	}
	bad = chunk
	bad.Data = make([]byte, p.MaxChunk+1)
	if code := call(t, s, "POST", p.NodeResultPath, c.Nodes[0].Token, bad, nil); code != 400 {
		t.Fatal(code)
	}
	bad = chunk
	bad.Data = []byte("abce")
	if code := call(t, s, "POST", p.NodeResultPath, c.Nodes[0].Token, bad, nil); code != 409 {
		t.Fatal(code)
	}
	probe := chunk
	probe.Probe, probe.Data = true, nil
	var ack p.ChunkAck
	if code := call(t, s, "POST", p.NodeResultPath, c.Nodes[0].Token, probe, &ack); code != 200 || ack.Offset != 0 || ack.Complete {
		t.Fatalf("digest failure must reset, not acknowledge: %d %+v", code, ack)
	}
	if code := call(t, s, "POST", p.NodeResultPath, c.Nodes[0].Token, chunk, &ack); code != 200 || !ack.Complete {
		t.Fatalf("%d %+v", code, ack)
	}
}
func TestLongHeartbeatDoesNotDelayDispatchOrCancellation(t *testing.T) {
	s, c, _ := fixture(t)
	input := v2Sync("node-a")
	input.WaitSeconds = 2700
	startPoll := func(input p.SyncRequest) <-chan *httptest.ResponseRecorder {
		t.Helper()
		body, _ := json.Marshal(input)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		t.Cleanup(cancel)
		r := httptest.NewRequest("POST", p.NodeControlPath, bytes.NewReader(body)).WithContext(ctx)
		r.Header.Set("Authorization", "Bearer "+c.Nodes[0].Token)
		result := make(chan *httptest.ResponseRecorder, 1)
		go func() { w := httptest.NewRecorder(); s.NodeHandler().ServeHTTP(w, r); result <- w }()
		return result
	}
	poll := startPoll(input)
	deadline := time.Now().Add(time.Second)
	for {
		s.mu.Lock()
		seen := s.nodes["node-a"].info.LastSeen
		s.mu.Unlock()
		if !seen.IsZero() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("long poll not registered")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case <-poll:
		t.Fatal("empty long poll returned immediately")
	default:
	}
	var task p.Task
	call(t, s, "POST", "/v1/tasks", c.AdminToken, taskInput(), &task)
	select {
	case w := <-poll:
		var response p.SyncResponse
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &response) != nil || response.Task == nil || response.Task.ID != task.ID {
			t.Fatalf("dispatch: %s", w.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("dispatch waited for heartbeat")
	}
	input.Report = &p.Report{TaskID: task.ID, State: "running"}
	poll = startPoll(input)
	call(t, s, "POST", "/v1/tasks/"+task.ID+"/cancel", c.AdminToken, nil, nil)
	select {
	case w := <-poll:
		var response p.SyncResponse
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &response) != nil || response.CancelTaskID != task.ID {
			t.Fatalf("cancel: %s", w.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("cancel waited for heartbeat")
	}
}
func TestLegacyWireAndMetricsIsolation(t *testing.T) {
	s, c, _ := fixture(t)
	call(t, s, "POST", "/v1/tasks", c.AdminToken, taskInput(), nil)
	var raw map[string]json.RawMessage
	call(t, s, "POST", p.NodeSyncPath, c.Nodes[0].Token, syncInput("node-a"), &raw)
	var task map[string]json.RawMessage
	json.Unmarshal(raw["task"], &task)
	for _, field := range []string{"timing", "upload", "result_pending"} {
		if _, exists := task[field]; exists {
			t.Fatalf("v1 response leaked %s", field)
		}
	}
	for _, token := range []string{c.AdminToken, c.Nodes[0].Token} {
		r := httptest.NewRequest("GET", "/v1/metrics", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		s.NodeHandler().ServeHTTP(w, r)
		if w.Code != 404 {
			t.Fatal("metrics exposed on node listener")
		}
	}
	var metrics p.MetricsSnapshot
	if code := call(t, s, "GET", "/v1/metrics", c.AdminToken, nil, &metrics); code != 200 || metrics.Controller.Control.Requests == 0 || metrics.Controller.StateWrites == 0 {
		t.Fatalf("metrics: %d %+v", code, metrics)
	}
}

func TestV1LostFinalAckCanFinishThroughV2Upload(t *testing.T) {
	s, c, _ := fixture(t)
	var task p.Task
	call(t, s, "POST", "/v1/tasks", c.AdminToken, taskInput(), &task)
	in := syncInput("node-a")
	call(t, s, "POST", p.NodeSyncPath, c.Nodes[0].Token, in, nil)
	result := &p.Result{Stdout: []byte("legacy result")}
	in.Report = &p.Report{TaskID: task.ID, State: "succeeded", Result: result}
	call(t, s, "POST", p.NodeSyncPath, c.Nodes[0].Token, in, nil)
	manifest := p.OutputManifest(result)
	in.Version, in.AcceptTask, in.LeaseSeconds = p.LatestVersion, true, 9000
	in.Report.Result = &p.Result{}
	in.Report.Upload = &manifest
	if code := call(t, s, "POST", p.NodeControlPath, c.Nodes[0].Token, in, nil); code != 200 {
		t.Fatal(code)
	}
	chunk := p.ResultChunk{Version: p.LatestVersion, NodeID: "node-a", InstanceID: in.InstanceID, TaskID: task.ID, SHA256: manifest.SHA256, Probe: true}
	var ack p.ChunkAck
	if code := call(t, s, "POST", p.NodeResultPath, c.Nodes[0].Token, chunk, &ack); code != 200 || !ack.Complete || ack.Offset != manifest.Total() {
		t.Fatalf("legacy ack not reconciled: %d %+v", code, ack)
	}
}
