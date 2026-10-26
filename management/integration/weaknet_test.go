package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tack.local/management/control"
	"tack.local/management/node"
	p "tack.local/management/protocol"
	"tack.local/management/protocol/shaping"
)

func TestOutputHelper(t *testing.T) {
	mode := os.Getenv("TACK_WEAKNET_HELPER")
	if mode == "" {
		return
	}
	f, err := os.OpenFile(os.Getenv("TACK_WEAKNET_MARKER"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		os.Exit(2)
	}
	_, err = f.WriteString("x")
	f.Close()
	if err != nil {
		os.Exit(3)
	}
	if mode == "wait" {
		time.Sleep(30 * time.Second)
	}
	if mode == "max-output" {
		os.Stdout.Write(bytes.Repeat([]byte("o"), p.MaxOutput))
		os.Stderr.Write(bytes.Repeat([]byte("e"), p.MaxOutput))
		os.Exit(0)
	}
	if mode == "severe-output" {
		os.Stdout.Write(bytes.Repeat([]byte("o"), 32*1024))
		os.Stderr.Write([]byte("separate stderr"))
		os.Exit(0)
	}
	os.Stdout.Write(bytes.Repeat([]byte("o"), 160*1024))
	os.Stderr.Write([]byte("separate stderr"))
	os.Exit(0)
}

type networkFixture struct {
	t          *testing.T
	api        *control.Server
	admin      *httptest.Server
	nodeServer *httptest.Server
	statePath  string
	config     p.ControlConfig
	nodeConfig p.NodeConfig
}

func newNetworkFixture(t *testing.T, wrap func(http.Handler) http.Handler) *networkFixture {
	return newNetworkFixtureHTTP2(t, wrap, false)
}
func newNetworkFixtureHTTP2(t *testing.T, wrap func(http.Handler) http.Handler, http2 bool, configure ...func(*p.ControlConfig)) *networkFixture {
	return newNetworkFixtureHTTP2Timeout(t, wrap, http2, 500*time.Millisecond, configure...)
}

func newNetworkFixtureHTTP2Timeout(t *testing.T, wrap func(http.Handler) http.Handler, http2 bool, writeTimeout time.Duration, configure ...func(*p.ControlConfig)) *networkFixture {
	t.Helper()
	c := p.ControlConfig{AdminToken: strings.Repeat("a", 40), Nodes: []p.NodeCredential{{ID: "node-a", Token: strings.Repeat("b", 40)}}}
	for _, setup := range configure {
		setup(&c)
	}
	statePath := filepath.Join(t.TempDir(), "state.json")
	api, err := control.New(c, statePath)
	if err != nil {
		t.Fatal(err)
	}
	handler := api.NodeHandler()
	if wrap != nil {
		handler = wrap(handler)
	}
	server := httptest.NewUnstartedServer(handler)
	server.EnableHTTP2 = http2
	server.Config.ReadTimeout = 20 * time.Second
	server.Config.WriteTimeout = writeTimeout
	shape := os.Getenv("TACK_TRAFFIC_SHAPING")
	var certificate []byte
	if shaping.Enabled(shape) {
		if http2 {
			t.Fatal("shaping fixture requires HTTP/1.1")
		}
		seed := httptest.NewTLSServer(nil)
		certificate = seed.Certificate().Raw
		server.Listener = shaping.TLSListener(server.Listener, seed.TLS)
		seed.Close()
		server.Config.Handler = shaping.ConfigureServer(server.Config, handler)
		server.Start()
		server.URL = strings.Replace(server.URL, "http://", "https://", 1)
	} else {
		server.StartTLS()
		certificate = server.Certificate().Raw
	}
	t.Cleanup(server.Close)
	admin := httptest.NewServer(api.AdminHandler())
	t.Cleanup(admin.Close)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate}), 0600); err != nil {
		t.Fatal(err)
	}
	return &networkFixture{nodeServer: server, statePath: statePath, t: t, api: api, admin: admin, config: c, nodeConfig: p.NodeConfig{Server: server.URL, NodeID: "node-a", Token: c.Nodes[0].Token, StateDir: t.TempDir(), WorkDir: t.TempDir(), CAFile: ca, MaxPendingResults: 2, TLSProfile: os.Getenv("TACK_TLS_PROFILE"), TrafficShaping: shape}}
}
func (f *networkFixture) request(method, path string, input, output any) {
	f.t.Helper()
	b, _ := json.Marshal(input)
	req, _ := http.NewRequest(method, f.admin.URL+path, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+f.config.AdminToken)
	resp, err := f.admin.Client().Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		f.t.Fatalf("HTTP %d: %s", resp.StatusCode, b)
	}
	if output != nil {
		if err := json.NewDecoder(resp.Body).Decode(output); err != nil {
			f.t.Fatal(err)
		}
	}
}
func (f *networkFixture) start() func() {
	f.t.Helper()
	a, err := node.New(f.nodeConfig)
	if err != nil {
		f.t.Fatal(err)
	}
	a.Logf = nil
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					f.t.Error(err)
				}
			case <-time.After(5 * time.Second):
				f.t.Error("node failed to stop")
			}
		})
	}
	f.t.Cleanup(stop)
	return stop
}
func (f *networkFixture) submit(mode, marker string) p.Task {
	f.t.Helper()
	exe, err := os.Executable()
	if err != nil {
		f.t.Fatal(err)
	}
	in := p.CreateTask{NodeID: "node-a", Command: p.Command{Mode: "exec", Program: exe, Args: []string{"-test.run=^TestOutputHelper$"}, Env: map[string]string{"TACK_WEAKNET_HELPER": mode, "TACK_WEAKNET_MARKER": marker, "GORACE": "atexit_sleep_ms=0"}, TimeoutSeconds: 40, OutputLimit: 200 * 1024}}
	if mode == "max-output" {
		in.Command.OutputLimit = p.MaxOutput
	}
	var task p.Task
	f.request("POST", "/v1/tasks", in, &task)
	return task
}
func eventually(t *testing.T, description string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("timed out: " + description)
}
func (f *networkFixture) task(id string, check func(p.Task) bool) p.Task {
	f.t.Helper()
	var result p.Task
	eventually(f.t, "task "+id, func() bool { result = p.Task{}; f.request("GET", "/v1/tasks/"+id, nil, &result); return check(result) })
	return result
}
func (f *networkFixture) metrics(check func(p.MetricsSnapshot) bool) p.MetricsSnapshot {
	f.t.Helper()
	var result p.MetricsSnapshot
	eventually(f.t, "node metrics", func() bool {
		result = p.MetricsSnapshot{}
		f.request("GET", "/v1/metrics", nil, &result)
		return check(result)
	})
	return result
}
func TestBlockedUploadStillDispatchesAndCancelsWithLongHeartbeat(t *testing.T) {
	gate := make(chan struct{})
	blocked := make(chan struct{})
	var once sync.Once
	var dropped, unavailable, resumed atomic.Bool
	var controls atomic.Int64
	f := newNetworkFixture(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == p.NodeRetentionPath {
				controls.Add(1)
			}
			if r.URL.Path != p.NodeResultPath {
				next.ServeHTTP(w, r)
				return
			}
			body, err := io.ReadAll(r.Body)
			if err != nil {
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			var chunk p.ResultChunk
			if json.Unmarshal(body, &chunk) != nil {
				t.Error("invalid chunk JSON")
				return
			}
			if chunk.Probe && dropped.Load() && unavailable.CompareAndSwap(false, true) {
				http.Error(w, "injected transient failure", 503)
				return
			}
			if len(chunk.Data) > 0 && !dropped.Load() {
				once.Do(func() { close(blocked) })
				select {
				case <-gate:
				case <-r.Context().Done():
					return
				}
				capture := httptest.NewRecorder()
				next.ServeHTTP(capture, r)
				if capture.Code != 200 {
					t.Errorf("chunk before lost ack: %d", capture.Code)
				}
				if dropped.CompareAndSwap(false, true) {
					panic(http.ErrAbortHandler)
				}
			} else {
				if chunk.Offset == p.MaxChunk && len(chunk.Data) > 0 {
					resumed.Store(true)
				}
				// Add bounded response delay without blocking the control connection.
				select {
				case <-time.After(25 * time.Millisecond):
				case <-r.Context().Done():
					return
				}
				next.ServeHTTP(w, r)
			}
		})
	})
	f.start()
	t.Cleanup(func() {
		select {
		case <-gate:
		default:
			close(gate)
		}
	})
	f.metrics(func(m p.MetricsSnapshot) bool { return len(m.Nodes) > 0 && m.Nodes[0].Metrics != nil })
	marker := filepath.Join(t.TempDir(), "once")
	started := time.Now()
	first := f.submit("output", marker)
	select {
	case <-blocked:
	case <-time.After(20 * time.Second):
		t.Fatal("upload did not block")
	}
	f.task(first.ID, func(task p.Task) bool { return task.State == "succeeded" && task.ResultPending })
	before := controls.Load()
	second := f.submit("wait", filepath.Join(t.TempDir(), "wait-marker"))
	f.task(second.ID, func(task p.Task) bool { return task.State == "running" })
	cancelStart := time.Now()
	f.request("POST", "/v1/tasks/"+second.ID+"/cancel", nil, nil)
	canceled := f.task(second.ID, func(task p.Task) bool { return task.State == "canceled" })
	cancelElapsed := time.Since(cancelStart)
	if controls.Load() <= before {
		t.Fatal("no control traffic during blocked upload")
	}
	metrics := f.metrics(func(m p.MetricsSnapshot) bool {
		return len(m.Nodes) > 0 && m.Nodes[0].Metrics != nil && m.Nodes[0].Metrics.PendingResults == 2 && !m.Nodes[0].Metrics.AcceptingTasks
	})
	if !metrics.Nodes[0].Online || metrics.Nodes[0].LeaseExpiresAt.Sub(metrics.Nodes[0].LastSeen) < 2*time.Hour {
		t.Fatal("long heartbeat lease not applied")
	}
	// At the configured backlog limit, new work remains queued until output drains.
	third := f.submit("output", filepath.Join(t.TempDir(), "third-marker"))
	time.Sleep(80 * time.Millisecond)
	var queued p.Task
	f.request("GET", "/v1/tasks/"+third.ID, nil, &queued)
	if queued.State != "queued" {
		t.Fatalf("backpressure dispatched task: %s", queued.State)
	}
	close(gate)
	final := f.task(first.ID, func(task p.Task) bool { return task.State == "succeeded" && !task.ResultPending })
	if !bytes.Equal(final.Result.Stdout, bytes.Repeat([]byte("o"), 160*1024)) || string(final.Result.Stderr) != "separate stderr" {
		t.Fatal("output changed during resume")
	}
	f.task(third.ID, func(task p.Task) bool { return task.State == "succeeded" && !task.ResultPending })
	if !dropped.Load() || !unavailable.Load() || !resumed.Load() {
		t.Fatal("fault/resume path was not exercised")
	}
	b, err := os.ReadFile(marker)
	if err != nil || string(b) != "x" {
		t.Fatalf("task re-executed: %q %v", b, err)
	}
	metrics = f.metrics(func(m p.MetricsSnapshot) bool {
		return m.Nodes[0].Metrics.Runtime.Upload.Failures >= 2 && m.Nodes[0].Metrics.PendingResults == 0
	})
	t.Logf("blocked upload cancellation=%s, controller-observed cancellation=%dms, transfer=%dms, elapsed=%s, upload failures=%d", cancelElapsed, canceled.Timing.CancelToTerminalMS, final.Timing.ResultTransferMS, time.Since(started), metrics.Nodes[0].Metrics.Runtime.Upload.Failures)
}
func TestNodeRestartResumesOutputWithoutReexecution(t *testing.T) {
	accepted := make(chan struct{})
	var intercepted atomic.Bool
	var resumed atomic.Bool
	f := newNetworkFixture(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != p.NodeResultPath {
				next.ServeHTTP(w, r)
				return
			}
			body, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(body))
			var chunk p.ResultChunk
			json.Unmarshal(body, &chunk)
			if len(chunk.Data) > 0 && intercepted.CompareAndSwap(false, true) {
				capture := httptest.NewRecorder()
				next.ServeHTTP(capture, r)
				if capture.Code != 200 {
					t.Errorf("durable chunk: %s", capture.Body.String())
				}
				close(accepted)
				<-r.Context().Done()
				return
			}
			if chunk.Offset == p.MaxChunk && len(chunk.Data) > 0 {
				resumed.Store(true)
			}
			next.ServeHTTP(w, r)
		})
	})
	stop := f.start()
	marker := filepath.Join(t.TempDir(), "once")
	task := f.submit("output", marker)
	select {
	case <-accepted:
	case <-time.After(20 * time.Second):
		t.Fatal("chunk not accepted")
	}
	stop()
	// Reuses the persisted instance identity: no multi-hour lease wait on restart.
	f.start()
	final := f.task(task.ID, func(task p.Task) bool {
		return !task.ResultPending && task.Result != nil && len(task.Result.Stdout) == 160*1024
	})
	b, err := os.ReadFile(marker)
	if err != nil || string(b) != "x" || !resumed.Load() {
		t.Fatalf("restart output/replay: %q %v resumed=%v", b, err, resumed.Load())
	}
	if string(final.Result.Stderr) != "separate stderr" {
		t.Fatal(fmt.Sprint(final.Result))
	}
}

func TestIdleControlOutlivesShortServerWriteTimeout(t *testing.T) {
	f := newNetworkFixture(t, nil)
	f.start()
	f.metrics(func(m p.MetricsSnapshot) bool { return m.Nodes[0].Metrics != nil })
	// The fixture has a 500ms default WriteTimeout. Authenticated long polls extend it.
	time.Sleep(750 * time.Millisecond)
	task := f.submit("output", filepath.Join(t.TempDir(), "once"))
	done := f.task(task.ID, func(task p.Task) bool { return task.State == "succeeded" && !task.ResultPending })
	if done.Timing == nil || done.Timing.DispatchedAt.IsZero() {
		t.Fatal("long poll never dispatched")
	}
	f.metrics(func(m p.MetricsSnapshot) bool {
		return m.Nodes[0].Metrics != nil && m.Nodes[0].Metrics.PendingResults == 0
	})
	var m p.MetricsSnapshot
	f.request("GET", "/v1/metrics", nil, &m)
	if m.Nodes[0].Metrics.Runtime.Control.Failures != 0 {
		t.Fatalf("idle control connection failed: %+v", m.Nodes[0].Metrics.Runtime.Control)
	}
}
