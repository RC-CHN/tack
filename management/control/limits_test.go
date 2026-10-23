package control

import (
	"net/http/httptest"
	"strings"
	p "tack.local/management/protocol"
	"testing"
)

func blockRequest(t *testing.T, s *Server, path, token string) func() {
	t.Helper()
	body := &blockedBody{started: make(chan struct{}), release: make(chan struct{})}
	r := httptest.NewRequest("POST", path, body)
	r.Header.Set("Authorization", "Bearer "+token)
	done := make(chan struct{})
	go func() { defer close(done); s.ServeHTTP(httptest.NewRecorder(), r) }()
	<-body.started
	return func() { close(body.release); <-done }
}

func TestUploadPressureLeavesControlAvailable(t *testing.T) {
	_, c, path := fixture(t)
	c.MaxConcurrentUploads = 1
	s, err := New(c, path)
	if err != nil {
		t.Fatal(err)
	}
	release := blockRequest(t, s, p.NodeResultPath, c.Nodes[0].Token)
	if code := call(t, s, "POST", p.NodeResultPath, c.Nodes[0].Token, nil, nil); code != 503 {
		release()
		t.Fatal(code)
	}
	if code := call(t, s, "POST", p.NodeControlPath, c.Nodes[0].Token, v2Sync("node-a"), nil); code != 200 {
		release()
		t.Fatal("upload pressure blocked control", code)
	}
	release()
	if code := call(t, s, "POST", p.NodeResultPath, c.Nodes[0].Token, nil, nil); code != 400 {
		t.Fatal("upload slot was not released", code)
	}
}

func TestControlConcurrencyBoundIsPerNode(t *testing.T) {
	s, c, _ := fixture(t)
	a := blockRequest(t, s, p.NodeControlPath, c.Nodes[0].Token)
	defer a()
	b := blockRequest(t, s, p.NodeControlPath, c.Nodes[0].Token)
	defer b()
	if code := call(t, s, "POST", p.NodeControlPath, c.Nodes[0].Token, nil, nil); code != 503 {
		t.Fatal(code)
	}
	if code := call(t, s, "POST", p.NodeControlPath, c.Nodes[1].Token, v2Sync("node-b"), nil); code != 200 {
		t.Fatal("one node exhausted another node's allowance", code)
	}
}

func TestNodeMetadataCannotRetainRequestSizedStrings(t *testing.T) {
	s, c, _ := fixture(t)
	for _, field := range []string{"os", "arch", "mode", "persistence", "metric"} {
		in := v2Sync("node-a")
		large := strings.Repeat("x", 4096)
		switch field {
		case "os":
			in.Node.OS = large
		case "arch":
			in.Node.Arch = large
		case "mode":
			in.Node.Modes = []string{large}
		case "persistence":
			in.Node.StatePersistence = large
		case "metric":
			in.Node.Metrics = &p.NodeMetrics{Runtime: p.RuntimeMetrics{Control: p.RequestMetrics{LastError: large}}}
		}
		if code := call(t, s, "POST", p.NodeControlPath, c.Nodes[0].Token, in, nil); code != 400 {
			t.Fatal(field, code)
		}
	}
}
