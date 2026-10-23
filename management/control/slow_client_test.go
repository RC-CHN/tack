package control

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	p "tack.local/management/protocol"
)

type blockedResponse struct {
	header  http.Header
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *blockedResponse) Header() http.Header { return w.header }
func (w *blockedResponse) WriteHeader(int)     {}
func (w *blockedResponse) Write(b []byte) (int, error) {
	w.once.Do(func() { close(w.started) })
	<-w.release
	return len(b), nil
}
func concurrentAdmin(t *testing.T, s *Server, token, path string) {
	t.Helper()
	r := httptest.NewRequest("GET", path, nil)
	r.Header.Set("Authorization", "Bearer "+token)
	done := make(chan int, 1)
	go func() { w := httptest.NewRecorder(); s.AdminHandler().ServeHTTP(w, r); done <- w.Code }()
	select {
	case code := <-done:
		if code != 200 {
			t.Fatal(code)
		}
	case <-time.After(time.Second):
		t.Fatal("slow peer held the global task queue mutex")
	}
}
func TestSlowNodeResponseDoesNotBlockQueue(t *testing.T) {
	s, c, _ := fixture(t)
	call(t, s, "POST", "/v1/tasks", c.AdminToken, taskInput(), nil)
	body, _ := json.Marshal(v2Sync("node-a"))
	r := httptest.NewRequest("POST", p.NodeControlPath, bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+c.Nodes[0].Token)
	w := &blockedResponse{header: http.Header{}, started: make(chan struct{}), release: make(chan struct{})}
	done := make(chan struct{})
	defer func() { close(w.release); <-done }()
	go func() { defer close(done); s.NodeHandler().ServeHTTP(w, r) }()
	<-w.started
	concurrentAdmin(t, s, c.AdminToken, "/v1/metrics")
}

type blockedBody struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockedBody) Read([]byte) (int, error) {
	b.once.Do(func() { close(b.started) })
	<-b.release
	return 0, io.EOF
}
func TestSlowAdministratorBodyDoesNotBlockQueue(t *testing.T) {
	s, c, _ := fixture(t)
	body := &blockedBody{started: make(chan struct{}), release: make(chan struct{})}
	r := httptest.NewRequest("POST", "/v1/tasks", body)
	r.Header.Set("Authorization", "Bearer "+c.AdminToken)
	done := make(chan struct{})
	defer func() { close(body.release); <-done }()
	go func() { defer close(done); s.AdminHandler().ServeHTTP(httptest.NewRecorder(), r) }()
	<-body.started
	concurrentAdmin(t, s, c.AdminToken, "/v1/tasks")
}
