package control

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	p "tack.local/management/protocol"
)

func fixture(t *testing.T) (*Server, p.ControlConfig, string) {
	t.Helper()
	c := p.ControlConfig{AdminToken: strings.Repeat("a", 40), Nodes: []p.NodeCredential{{ID: "node-a", Token: strings.Repeat("b", 40)}, {ID: "node-b", Token: strings.Repeat("c", 40)}}}
	path := filepath.Join(t.TempDir(), "state.json")
	s, err := New(c, path)
	if err != nil {
		t.Fatal(err)
	}
	return s, c, path
}
func call(t *testing.T, s *Server, method, path, token string, input, output any) int {
	t.Helper()
	b, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(method, path, bytes.NewReader(b))
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if output != nil && w.Code < 300 {
		reflect.ValueOf(output).Elem().SetZero()
		if err := json.Unmarshal(w.Body.Bytes(), output); err != nil {
			t.Fatal(err)
		}
	}
	return w.Code
}

func TestPublicListenerHasNoAdminRoutes(t *testing.T) {
	s, c, _ := fixture(t)
	for _, token := range []string{c.AdminToken, c.Nodes[0].Token} {
		r := httptest.NewRequest("GET", "/v1/tasks", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		s.NodeHandler().ServeHTTP(w, r)
		if w.Code != 404 {
			t.Fatalf("administrator route exposed with HTTP %d", w.Code)
		}
	}
}
func taskInput() p.CreateTask {
	return p.CreateTask{NodeID: "node-a", Command: p.Command{Mode: "exec", Program: "echo", Args: []string{"hello"}}}
}
func syncInput(id string) p.SyncRequest {
	return p.SyncRequest{Version: p.Version, InstanceID: "instance-" + id, Node: p.Node{ID: id}}
}

func TestAuthorizationIsolationAndDurableQueue(t *testing.T) {
	s, c, path := fixture(t)
	if code := call(t, s, "POST", "/v1/tasks", c.Nodes[0].Token, taskInput(), nil); code != 401 {
		t.Fatal(code)
	}
	var task p.Task
	if code := call(t, s, "POST", "/v1/tasks", c.AdminToken, taskInput(), &task); code != 201 {
		t.Fatal(code)
	}
	var response p.SyncResponse
	if code := call(t, s, "POST", p.NodeSyncPath, c.Nodes[1].Token, syncInput("node-b"), &response); code != 200 || response.Task != nil {
		t.Fatalf("%d %+v", code, response)
	}
	if code := call(t, s, "POST", p.NodeSyncPath, c.Nodes[0].Token, syncInput("node-a"), &response); code != 200 || response.Task.ID != task.ID {
		t.Fatalf("%d %+v", code, response)
	}
	bad := syncInput("node-b")
	bad.Report = &p.Report{TaskID: task.ID, State: "running"}
	if code := call(t, s, "POST", p.NodeSyncPath, c.Nodes[1].Token, bad, nil); code != 403 {
		t.Fatal(code)
	}
	restarted, err := New(c, path)
	if err != nil {
		t.Fatal(err)
	}
	if code := call(t, restarted, "POST", p.NodeSyncPath, c.Nodes[0].Token, syncInput("node-a"), &response); code != 200 || response.Task.ID != task.ID {
		t.Fatal("restart lost active task")
	}
	done := syncInput("node-a")
	done.Report = &p.Report{TaskID: task.ID, State: "succeeded", Result: &p.Result{Stdout: []byte("ok")}}
	for i := 0; i < 2; i++ {
		if code := call(t, restarted, "POST", p.NodeSyncPath, c.Nodes[0].Token, done, &response); code != 200 || response.Acknowledged != task.ID || response.Task != nil {
			t.Fatalf("%d %+v", code, response)
		}
	}
}

func TestCancelAndOversizeResults(t *testing.T) {
	s, c, _ := fixture(t)
	var task p.Task
	call(t, s, "POST", "/v1/tasks", c.AdminToken, taskInput(), &task)
	call(t, s, "POST", "/v1/tasks/"+task.ID+"/cancel", c.AdminToken, nil, &task)
	if task.State != "canceled" {
		t.Fatal(task.State)
	}
	var response p.SyncResponse
	call(t, s, "POST", p.NodeSyncPath, c.Nodes[0].Token, syncInput("node-a"), &response)
	if response.Task != nil {
		t.Fatal("canceled queued task dispatched")
	}
	call(t, s, "POST", "/v1/tasks", c.AdminToken, taskInput(), &task)
	call(t, s, "POST", p.NodeSyncPath, c.Nodes[0].Token, syncInput("node-a"), &response)
	input := syncInput("node-a")
	input.Report = &p.Report{TaskID: task.ID, State: "succeeded", Result: &p.Result{Stdout: make([]byte, task.Command.OutputLimit+1)}}
	if code := call(t, s, "POST", p.NodeSyncPath, c.Nodes[0].Token, input, nil); code != 400 {
		t.Fatal(code)
	}
	call(t, s, "POST", "/v1/tasks/"+task.ID+"/cancel", c.AdminToken, nil, nil)
	call(t, s, "POST", p.NodeSyncPath, c.Nodes[0].Token, syncInput("node-a"), &response)
	if response.CancelTaskID != task.ID {
		t.Fatal("running cancellation not delivered")
	}
}

func TestConcurrentNodeIdentityRejected(t *testing.T) {
	s, c, _ := fixture(t)
	input := syncInput("node-a")
	call(t, s, "POST", p.NodeSyncPath, c.Nodes[0].Token, input, nil)
	input.InstanceID = "another-instance"
	if code := call(t, s, "POST", p.NodeSyncPath, c.Nodes[0].Token, input, nil); code != 409 {
		t.Fatal(code)
	}
}
