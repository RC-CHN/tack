package control

import (
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	p "tack.local/management/protocol"
)

func newSubmission(t *testing.T) p.CreateTask {
	t.Helper()
	in := taskInput()
	var err error
	in.RequestID, err = p.NewRequestID()
	if err != nil {
		t.Fatal(err)
	}
	return in
}
func TestPersistentSubmissionIdempotencyAndConflict(t *testing.T) {
	s, c, path := fixture(t)
	in := newSubmission(t)
	var first p.Task
	if code := call(t, s, "POST", "/v1/tasks", c.AdminToken, in, &first); code != 201 {
		t.Fatal(code)
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var got p.Task
			if code := call(t, s, "POST", "/v1/tasks", c.AdminToken, in, &got); code != 200 || got.ID != first.ID || got.Sequence != first.Sequence {
				t.Errorf("retry: %d %+v", code, got)
			}
		}()
	}
	wg.Wait()
	restarted, err := New(c, path)
	if err != nil {
		t.Fatal(err)
	}
	var got p.Task
	if code := call(t, restarted, "POST", "/v1/tasks", c.AdminToken, in, &got); code != 200 || got.ID != first.ID {
		t.Fatalf("restart: %d %+v", code, got)
	}
	variants := []p.CreateTask{in, in, in, in}
	variants[0].NodeID = "node-b"
	variants[1].Command.Args = []string{"changed"}
	variants[2].CallerID = "different"
	deadline := time.Now().Add(time.Hour)
	variants[3].StartBefore = &deadline
	for _, v := range variants {
		if code := call(t, restarted, "POST", "/v1/tasks", c.AdminToken, v, nil); code != 409 {
			t.Fatalf("conflict returned %d", code)
		}
	}
	if len(restarted.state.Tasks) != 1 || restarted.state.Sequences[in.NodeID] != 1 {
		t.Fatal("duplicate task or sequence")
	}
}
func TestSubmissionRetirementSurvivesPruningAndRestart(t *testing.T) {
	s, c, path := fixture(t)
	s.config.MaxHistoryTasks = 1
	c.MaxHistoryTasks = 1
	in := newSubmission(t)
	var first p.Task
	if call(t, s, "POST", "/v1/tasks", c.AdminToken, in, &first) != 201 {
		t.Fatal("submit")
	}
	if call(t, s, "POST", "/v1/tasks/"+first.ID+"/cancel", c.AdminToken, nil, nil) != 200 {
		t.Fatal("cancel")
	}
	if call(t, s, "POST", "/v1/tasks", c.AdminToken, taskInput(), nil) != 201 {
		t.Fatal("replacement")
	}
	if _, ok, err := s.lookupTask(first.ID); err != nil || ok {
		t.Fatal("history not reclaimed", err)
	}
	restarted, err := New(c, path)
	if err != nil {
		t.Fatal(err)
	}
	if code := call(t, restarted, "POST", "/v1/tasks", c.AdminToken, in, nil); code != 410 {
		t.Fatal("retired identity accepted", code)
	}
	if restarted.state.Sequences[in.NodeID] != 2 {
		t.Fatal("retry consumed sequence")
	}
}
func TestRetirementIsDurableBeforeDeletion(t *testing.T) {
	s, c, _ := fixture(t)
	in := newSubmission(t)
	var first p.Task
	call(t, s, "POST", "/v1/tasks", c.AdminToken, in, &first)
	call(t, s, "POST", "/v1/tasks/"+first.ID+"/cancel", c.AdminToken, nil, nil)
	s.config.MaxHistoryTasks = 1
	original := s.writeJSON
	s.writeJSON = func(path string, v any) error {
		if path == s.path {
			return errors.New("disk failed")
		}
		return original(path, v)
	}
	if code := call(t, s, "POST", "/v1/tasks", c.AdminToken, taskInput(), nil); code != 503 {
		t.Fatal(code)
	}
	if _, err := os.Stat(s.releasedPath(first.ID)); err != nil {
		t.Fatal("deleted without retirement checkpoint", err)
	}
	var got p.Task
	if code := call(t, s, "POST", "/v1/tasks", c.AdminToken, in, &got); code != 200 || got.ID != first.ID {
		t.Fatal("lost original", code)
	}
}
func TestDeadlineEnforcementAndLegacyNodeGate(t *testing.T) {
	s, c, _ := fixture(t)
	in := newSubmission(t)
	before := time.Now().Add(time.Hour)
	in.StartBefore = &before
	var task p.Task
	call(t, s, "POST", "/v1/tasks", c.AdminToken, in, &task)
	legacy := v2Sync("node-a")
	var response p.SyncResponse
	if code := call(t, s, "POST", p.NodeControlPath, c.Nodes[0].Token, legacy, &response); code != 200 || response.Task != nil {
		t.Fatal("legacy dispatched deadline task", code)
	}
	current := legacy
	current.Version = p.OperationsVersion
	if code := call(t, s, "POST", p.NodeOperationsPath, c.Nodes[0].Token, current, &response); code != 200 || response.Task == nil || response.Task.StartBefore == nil {
		t.Fatal("new node did not get deadline", code)
	}
	queued := newSubmission(t)
	queued.StartBefore = &before
	var expired p.Task
	call(t, s, "POST", "/v1/tasks", c.AdminToken, queued, &expired)
	s.tx.Lock()
	err := s.expireQueued(before.Add(time.Second))
	s.tx.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	var got p.Task
	if code := call(t, s, "GET", "/v1/tasks/"+expired.ID, c.AdminToken, nil, &got); code != 200 || got.State != "expired" || got.Result.ErrorCode != "start_deadline_exceeded" {
		t.Fatalf("expiry: %d %+v", code, got)
	}
	if s.state.Tasks[task.ID].State != "dispatched" {
		t.Fatal("controller canceled already dispatched work")
	}
	if _, err := os.Stat(s.releasedPath(expired.ID)); err != nil {
		t.Fatal("never-dispatched expiry not reclaimable", err)
	}
}
func TestOperatorScopeRotationRevocationAndFiltering(t *testing.T) {
	s, c, path := fixture(t)
	token := strings.Repeat("d", 40)
	op := p.Operator{ID: "agent-one", Token: token, NodeIDs: []string{"node-a"}, Read: true, Execute: true}
	if code := call(t, s, "POST", "/v1/operators", c.AdminToken, op, nil); code != 200 {
		t.Fatal(code)
	}
	in := newSubmission(t)
	in.CallerID = "batch-one"
	var own p.Task
	if call(t, s, "POST", "/v1/tasks", token, in, &own) != 201 || own.Owner != op.ID {
		t.Fatal("operator submit")
	}
	other := taskInput()
	other.NodeID = "node-b"
	var hidden p.Task
	call(t, s, "POST", "/v1/tasks", c.AdminToken, other, &hidden)
	for _, path := range []string{"/v1/tasks/" + hidden.ID, "/v1/tasks/" + hidden.ID + "/cancel"} {
		method := "GET"
		if strings.HasSuffix(path, "cancel") {
			method = "POST"
		}
		if code := call(t, s, method, path, token, nil, nil); code != 404 {
			t.Fatal("scope leaked", code)
		}
	}
	if call(t, s, "POST", "/v1/tasks", token, other, nil) != 403 {
		t.Fatal("cross-node submission")
	}
	if call(t, s, "POST", "/v1/operators", token, op, nil) != 403 {
		t.Fatal("privilege escalation")
	}
	var nodes []p.Node
	call(t, s, "GET", "/v1/nodes", token, nil, &nodes)
	if len(nodes) != 1 || nodes[0].ID != "node-a" {
		t.Fatal("node list leaked")
	}
	var page p.TaskPage
	call(t, s, "GET", "/v2/tasks?caller_id=batch-one&owner=agent-one&state=queued", token, nil, &page)
	if len(page.Tasks) != 1 || page.Tasks[0].ID != own.ID {
		t.Fatal("filters", page)
	}
	call(t, s, "GET", "/v2/tasks?node=node-b", token, nil, &page)
	if len(page.Tasks) != 0 {
		t.Fatal("filter escaped scope")
	}
	op.Token = strings.Repeat("e", 40)
	if call(t, s, "POST", "/v1/operators", c.AdminToken, op, nil) != 200 {
		t.Fatal("rotation")
	}
	if call(t, s, "GET", "/v1/nodes", token, nil, nil) != 401 {
		t.Fatal("old credential survived rotation")
	}
	var retry p.Task
	if call(t, s, "POST", "/v1/tasks", op.Token, in, &retry) != 200 || retry.ID != own.ID {
		t.Fatal("rotation changed request namespace")
	}
	restarted, err := New(c, path)
	if err != nil {
		t.Fatal(err)
	}
	if call(t, restarted, "GET", "/v1/nodes", op.Token, nil, nil) != 200 {
		t.Fatal("credential lost after restart")
	}
	op.Execute = false
	call(t, restarted, "POST", "/v1/operators", c.AdminToken, op, nil)
	if call(t, restarted, "POST", "/v1/tasks", op.Token, newSubmission(t), nil) != 403 {
		t.Fatal("read-only executed")
	}
	if call(t, restarted, "DELETE", "/v1/operators/"+op.ID, c.AdminToken, nil, nil) != 200 || call(t, restarted, "GET", "/v1/nodes", op.Token, nil, nil) != 401 {
		t.Fatal("revocation ineffective")
	}
	var list []p.Operator
	call(t, restarted, "GET", "/v1/operators", c.AdminToken, nil, &list)
	if len(list) != 1 || list[0].Token != "" || !list[0].Disabled {
		t.Fatal("credential exposed or not disabled")
	}
}
func TestStatusSeparatesManagementAndTunnel(t *testing.T) {
	s, c, _ := fixture(t)
	in := v2Sync("node-a")
	in.Version = p.OperationsVersion
	in.Node.Status = &p.NodeStatus{AcceptingTasks: false, CurrentTask: "task-one", BacklogReason: "task_running", Tunnel: p.TunnelStatus{State: "disconnected", UpdatedAt: time.Now().UTC()}}
	if code := call(t, s, "POST", p.NodeOperationsPath, c.Nodes[0].Token, in, nil); code != 200 {
		t.Fatal(code)
	}
	var nodes []p.Node
	call(t, s, "GET", "/v1/nodes", c.AdminToken, nil, &nodes)
	status := nodes[0].Status
	if !nodes[0].Online || status.Management != "lease_valid" || status.Tunnel.State != "disconnected" || status.CurrentTask != "task-one" || status.AcceptingTasks {
		t.Fatalf("conflated status: %+v", status)
	}
}
func TestFilterHistoryPaginationAndTime(t *testing.T) {
	s, c, _ := fixture(t)
	for i := 0; i < 6; i++ {
		in := newSubmission(t)
		in.CallerID = fmt.Sprintf("batch-%d", i%2)
		var task p.Task
		call(t, s, "POST", "/v1/tasks", c.AdminToken, in, &task)
		call(t, s, "POST", "/v1/tasks/"+task.ID+"/cancel", c.AdminToken, nil, nil)
	}
	seen := map[string]bool{}
	cursor := ""
	for {
		var page p.TaskPage
		path := "/v2/tasks?limit=1&caller_id=batch-1&state=canceled&after=" + cursor
		if code := call(t, s, "GET", path, c.AdminToken, nil, &page); code != 200 {
			t.Fatal(code)
		}
		for _, task := range page.Tasks {
			if seen[task.ID] || task.CallerID != "batch-1" {
				t.Fatal("invalid page")
			}
			seen[task.ID] = true
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if len(seen) != 3 {
		t.Fatal(len(seen))
	}
	var page p.TaskPage
	call(t, s, "GET", "/v2/tasks?since=2099-01-01T00:00:00Z", c.AdminToken, nil, &page)
	if len(page.Tasks) != 0 {
		t.Fatal("time filter")
	}
}
func TestErrorsHaveStableEnvelope(t *testing.T) {
	s, c, _ := fixture(t)
	r := httptest.NewRequest("GET", "/unknown", nil)
	r.Header.Set("Authorization", "Bearer "+c.AdminToken)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if !strings.Contains(w.Body.String(), `"error_code":"not_found"`) || !strings.Contains(w.Body.String(), `"phase":"control"`) {
		t.Fatal(w.Body.String())
	}
}
