package integration_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	p "tack.local/management/protocol"
	"tack.local/testfixtures/faultnet"
)

// A real TLS stream crosses both fixtures. TCP delay/backpressure is distinct
// from packet loss: the lost acknowledgement is injected after durable commit.
func TestManagementThroughImpairedTransport(t *testing.T) {
	for _, h2 := range []bool{false, true} {
		name := "http1"
		if h2 {
			name = "http2"
		}
		t.Run(name, func(t *testing.T) {
			durable := make(chan struct{})
			release := make(chan struct{})
			defer close(release)
			var lost, resumed atomic.Bool
			var protocol2 atomic.Int64
			f := newNetworkFixtureHTTP2Timeout(t, func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.ProtoMajor == 2 {
						protocol2.Add(1)
					}
					if r.URL.Path == p.NodeResultPath {
						// This interceptor reads before the real upload handler can
						// extend its deadlines; use that handler's upload allowance.
						rc := http.NewResponseController(w)
						_ = rc.SetReadDeadline(time.Now().Add(p.ResultTimeoutSeconds * time.Second))
						_ = rc.SetWriteDeadline(time.Now().Add((p.ResultTimeoutSeconds + 20) * time.Second))
						body, err := io.ReadAll(r.Body)
						if err != nil {
							return
						}
						r.Body = io.NopCloser(bytes.NewReader(body))
						var chunk p.ResultChunk
						if err = json.Unmarshal(body, &chunk); err != nil {
							t.Error(err)
							return
						}
						if len(chunk.Data) > 0 && lost.CompareAndSwap(false, true) {
							capture := httptest.NewRecorder()
							next.ServeHTTP(capture, r)
							if capture.Code != 200 {
								t.Errorf("durable chunk: %d", capture.Code)
							}
							close(durable)
							select {
							case <-r.Context().Done():
							case <-release:
							}
							return
						}
						if chunk.Offset == p.MaxChunk && len(chunk.Data) > 0 {
							resumed.Store(true)
						}
					}
					next.ServeHTTP(w, r)
				})
			}, h2, 20*time.Second)
			relay, err := faultnet.NewConfigured(strings.TrimPrefix(f.nodeServer.URL, "https://"), faultnet.Options{Upload: faultnet.Traffic{Delay: 40 * time.Millisecond, BytesPerSecond: 64 * 1024}, Download: faultnet.Traffic{Delay: 40 * time.Millisecond, BytesPerSecond: 64 * 1024}, QueueChunks: 2})
			if err != nil {
				t.Fatal(err)
			}
			defer relay.Close()
			link, err := faultnet.New(relay.Addr())
			if err != nil {
				t.Fatal(err)
			}
			defer link.Close()
			f.nodeConfig.Server = "https://" + link.Addr()
			stop := f.start()
			defer stop()
			marker := filepath.Join(t.TempDir(), "once")
			first := f.submit("output", marker)
			select {
			case <-durable:
			case <-time.After(20 * time.Second):
				t.Fatal("upload never crossed impaired path")
			}
			link.SetMode(faultnet.Offline)
			second := f.submit("wait", filepath.Join(t.TempDir(), "cancel-once"))
			time.Sleep(200 * time.Millisecond)
			link.SetMode(faultnet.Online)
			f.task(second.ID, func(task p.Task) bool { return task.State == "running" })
			start := time.Now()
			f.request("POST", "/v1/tasks/"+second.ID+"/cancel", nil, nil)
			f.task(second.ID, func(task p.Task) bool { return task.State == "canceled" })
			if time.Since(start) > 8*time.Second {
				t.Fatal("cancellation starved behind output")
			}
			final := f.task(first.ID, func(task p.Task) bool { return task.State == "succeeded" && !task.ResultPending })
			if final.Result == nil || !bytes.Equal(final.Result.Stdout, bytes.Repeat([]byte("o"), 160*1024)) || string(final.Result.Stderr) != "separate stderr" {
				t.Fatal("resumed result corrupted")
			}
			data, err := os.ReadFile(marker)
			if err != nil || string(data) != "x" || !resumed.Load() {
				t.Fatalf("execution/offset recovery failed: %q %v resumed=%v", data, err, resumed.Load())
			}
			if h2 && protocol2.Load() == 0 {
				t.Fatal("HTTP/2 not negotiated")
			}
			// A silent established peer must not prevent shutdown.
			link.SetMode(faultnet.Paused)
			start = time.Now()
			stop()
			if time.Since(start) > 3*time.Second {
				t.Fatal("silent peer prevented shutdown")
			}
			link.Close()
			if link.Snapshot().Active != 0 {
				t.Fatal("fixture retained connections")
			}
			traffic := relay.Snapshot()
			t.Logf("stream bytes up=%d down=%d connections=%+v", traffic.UpBytes, traffic.DownBytes, link.Snapshot())
		})
	}
}
