package node

import (
	"bytes"
	"context"
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	p "tack.local/management/protocol"
)

func TestUploadSlicesPreserveStreamBoundaries(t *testing.T) {
	result := &p.Result{Stdout: bytes.Repeat([]byte("o"), 70000), Stderr: bytes.Repeat([]byte("e"), 80000)}
	all := append(append([]byte{}, result.Stdout...), result.Stderr...)
	for _, offset := range []int{0, p.MaxChunk, 69999, 70000, 70001, len(all) - 1} {
		end := offset + p.MaxChunk
		if end > len(all) {
			end = len(all)
		}
		if got := outputChunk(result, offset); !bytes.Equal(got, all[offset:end]) {
			t.Fatalf("incorrect chunk at %d", offset)
		}
	}
}

func waitResource(t *testing.T, message string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal(message)
}
func lifecycleHeap() (uint64, int, int) {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	fd := -1
	if runtime.GOOS == "linux" {
		if entries, err := os.ReadDir("/proc/self/fd"); err == nil {
			fd = len(entries)
		}
	}
	return m.HeapAlloc, runtime.NumGoroutine(), fd
}

// Repeatedly stop with BOTH transports blocked. This exercises context timers,
// client pools, server streams and retained upload buffers on the shutdown path.
func TestRepeatedShutdownReleasesConnectionsAndBuffers(t *testing.T) {
	for _, h2 := range []bool{false, true} {
		name := "http1"
		if h2 {
			name = "http2"
		}
		t.Run(name, func(t *testing.T) {
			var connections, active, protocol2 atomic.Int64
			entered := make(chan struct{}, 2)
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.ProtoMajor == 2 {
					protocol2.Add(1)
				}
				if r.URL.Path == "/health" {
					w.Header().Set("Content-Type", "application/json")
					w.Write([]byte(`{"protocol_version":2}`))
					return
				}
				// Consume the finite body before waiting so cancellation is observable on
				// HTTP/1 as well as HTTP/2 (the standard library monitors the idle reader).
				var value any
				if err := p.Decode(r.Body, &value); err != nil {
					t.Error(err)
					return
				}
				active.Add(1)
				defer active.Add(-1)
				entered <- struct{}{}
				<-r.Context().Done()
			}))
			server.EnableHTTP2 = h2
			server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
				switch state {
				case http.StateNew:
					connections.Add(1)
				case http.StateClosed:
					connections.Add(-1)
				}
			}
			server.StartTLS()
			defer server.Close()
			cfg := testConfig(t)
			cfg.Server = server.URL
			cfg.CAFile = filepath.Join(t.TempDir(), "ca.pem")
			if err := os.WriteFile(cfg.CAFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
				t.Fatal(err)
			}
			rec := journal{Task: p.Task{ID: "pending", NodeID: cfg.NodeID}, Report: p.Report{TaskID: "pending", State: "succeeded", Result: &p.Result{Stdout: bytes.Repeat([]byte("x"), p.MaxOutput), Stderr: bytes.Repeat([]byte("y"), p.MaxOutput)}}, MetadataAcknowledged: true}
			if err := p.WriteJSON(filepath.Join(cfg.StateDir, "journal.json"), map[string]journal{"pending": rec}); err != nil {
				t.Fatal(err)
			}
			cycle := func() {
				a, err := New(cfg)
				if err != nil {
					t.Fatal(err)
				}
				a.Logf = nil
				ctx, cancel := context.WithCancel(context.Background())
				done := make(chan error, 1)
				go func() { done <- a.Run(ctx) }()
				for i := 0; i < 2; i++ {
					select {
					case <-entered:
					case <-time.After(3 * time.Second):
						cancel()
						t.Fatal("both control and upload did not start")
					}
				}
				cancel()
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("shutdown blocked")
				}
				waitResource(t, "request handlers or connections survived shutdown", func() bool { return active.Load() == 0 && connections.Load() == 0 })
			}
			for i := 0; i < 4; i++ {
				cycle()
			}
			baseline, goroutines, fd := lifecycleHeap()
			for i := 0; i < 20; i++ {
				cycle()
			}
			retained, afterG, afterFD := lifecycleHeap()
			if int64(retained)-int64(baseline) > 4*1024*1024 || afterG > goroutines+2 || (fd >= 0 && afterFD > fd+2) {
				t.Fatalf("resource accumulation: heap %d -> %d; goroutines %d -> %d; fd %d -> %d", baseline, retained, goroutines, afterG, fd, afterFD)
			}
			if h2 && protocol2.Load() == 0 {
				t.Fatal("HTTP/2 not exercised")
			}
			t.Logf("24 blocked-request shutdowns: heap %d -> %d; goroutines %d -> %d; fd %d -> %d; all server connections closed", baseline, retained, goroutines, afterG, fd, afterFD)
		})
	}
}

func TestIntentWriteFailureNeverStartsTask(t *testing.T) {
	cfg := testConfig(t)
	a, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cfg.StateDir, "journal.json")
	exe, _ := os.Executable()
	marker := filepath.Join(t.TempDir(), "must-not-run")
	task := p.Task{ID: "cannot-persist", NodeID: cfg.NodeID, Command: p.Command{Mode: "exec", Program: exe, Args: []string{"-test.run=^TestHelperExecution$"}, Env: map[string]string{"TACK_TEST_HELPER": "1", "TACK_TEST_MARKER": marker}, TimeoutSeconds: 5, OutputLimit: 100}}
	a.client = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/health" {
			return response(map[string]int{"protocol_version": 1}), nil
		}
		if err := os.Remove(path); err != nil {
			t.Error(err)
		}
		if err := os.Mkdir(path, 0700); err != nil {
			t.Error(err)
		}
		return response(p.SyncResponse{Version: 1, Task: &task}), nil
	})}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := a.Run(ctx); err == nil {
		t.Fatal("intent write failure was ignored")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("task ran before its intent was durable")
	}
	entries, _ := os.ReadDir(cfg.StateDir)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".state-") {
			t.Fatal("temporary state file leaked after failed write")
		}
	}
}

func TestShutdownCancelsPendingDial(t *testing.T) {
	entered := make(chan struct{})
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	set := newConnectionSet()
	set.attach(&http.Client{Transport: transport})
	done := make(chan error, 1)
	go func() { _, err := transport.DialContext(context.Background(), "tcp", "unused:443"); done <- err }()
	<-entered
	closed := make(chan struct{})
	go func() { set.close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("shutdown left a pending dial")
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("pending dial succeeded unexpectedly")
		}
	case <-time.After(time.Second):
		t.Fatal("dial did not return")
	}
	if _, err := transport.DialContext(context.Background(), "tcp", "unused:443"); err == nil {
		t.Fatal("stopped owner accepted a new dial")
	}
}
