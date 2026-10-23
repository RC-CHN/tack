package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"tack.local/management/control"
	"tack.local/management/node"
	p "tack.local/management/protocol"
)

func TestHTTPSExecuteAndCancel(t *testing.T) {
	config := p.ControlConfig{AdminToken: strings.Repeat("a", 40), Nodes: []p.NodeCredential{{ID: "node-a", Token: strings.Repeat("b", 40)}}}
	api, err := control.New(config, filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(api.NodeHandler())
	defer server.Close()
	admin := httptest.NewServer(api.AdminHandler())
	defer admin.Close()
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	a, err := node.New(p.NodeConfig{Server: server.URL, NodeID: "node-a", Token: config.Nodes[0].Token, StateDir: t.TempDir(), WorkDir: t.TempDir(), CAFile: ca, PollSeconds: 1})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	request := func(method, path string, input, output any) {
		t.Helper()
		b, _ := json.Marshal(input)
		req, _ := http.NewRequest(method, admin.URL+path, bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+config.AdminToken)
		resp, err := admin.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 300 {
			t.Fatalf("HTTP %d", resp.StatusCode)
		}
		if output != nil {
			if err := json.NewDecoder(resp.Body).Decode(output); err != nil {
				t.Fatal(err)
			}
		}
	}
	waitTask := func(id, state string) p.Task {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			var task p.Task
			request("GET", "/v1/tasks/"+id, nil, &task)
			if task.State == state {
				return task
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatal("task did not reach " + state)
		return p.Task{}
	}
	mode, script := "posix-sh", "printf hello-management"
	if runtime.GOOS == "windows" {
		mode, script = "cmd", "@echo hello-management\r\n"
	}
	var task p.Task
	request("POST", "/v1/tasks", p.CreateTask{NodeID: "node-a", Command: p.Command{Mode: mode, Script: script}}, &task)
	task = waitTask(task.ID, "succeeded")
	if task.Result == nil || !strings.Contains(string(task.Result.Stdout), "hello-management") {
		t.Fatalf("%+v", task)
	}
	script = "sleep 30"
	if runtime.GOOS == "windows" {
		script = "@ping -n 30 127.0.0.1 >nul\r\n"
	}
	request("POST", "/v1/tasks", p.CreateTask{NodeID: "node-a", Command: p.Command{Mode: mode, Script: script}}, &task)
	waitTask(task.ID, "running")
	request("POST", "/v1/tasks/"+task.ID+"/cancel", nil, nil)
	waitTask(task.ID, "canceled")
}
