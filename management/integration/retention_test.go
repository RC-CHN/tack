package integration_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"tack.local/management/control"
	p "tack.local/management/protocol"
)

func TestRetentionChurnLostAcksAndControllerRollback(t *testing.T) {
	for _, h2 := range []bool{false, true} {
		name := "http1"
		if h2 {
			name = "http2"
		}
		t.Run(name, func(t *testing.T) {
			var replaced atomic.Pointer[control.Server]
			var lostFinal, lostReceipt atomic.Bool
			f := newNetworkFixtureHTTP2(t, func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					handler := next
					if api := replaced.Load(); api != nil {
						handler = api.NodeHandler()
					}
					capture := httptest.NewRecorder()
					handler.ServeHTTP(capture, r)
					if r.URL.Path == p.NodeResultPath && capture.Code == 200 {
						var ack p.ChunkAck
						json.Unmarshal(capture.Body.Bytes(), &ack)
						if ack.Complete && lostFinal.CompareAndSwap(false, true) {
							http.Error(w, "lost final ack", 503)
							return
						}
					}
					if r.URL.Path == p.NodeRetentionPath && capture.Code == 200 {
						var ack p.SyncResponse
						json.Unmarshal(capture.Body.Bytes(), &ack)
						if len(ack.Receipts) > 0 && lostReceipt.CompareAndSwap(false, true) {
							http.Error(w, "lost receipt ack", 503)
							return
						}
					}
					for k, v := range capture.Header() {
						w.Header()[k] = v
					}
					w.WriteHeader(capture.Code)
					w.Write(capture.Body.Bytes())
				})
			}, h2, func(c *p.ControlConfig) { c.MaxHistoryTasks = 4 })
			marker := filepath.Join(t.TempDir(), "executions")
			first := f.submit("output", marker)
			beforeDispatch, err := os.ReadFile(f.statePath)
			if err != nil {
				t.Fatal(err)
			}
			stop := f.start()
			const count = 24
			for i := 0; i < count; i++ {
				task := first
				if i > 0 {
					task = f.submit("output", marker)
				}
				f.task(task.ID, func(t p.Task) bool { return t.State == "succeeded" && !t.ResultPending })
				eventually(t, "durable receipt", func() bool {
					_, err := os.Stat(filepath.Join(f.statePath+".history", "released", task.ID+".json"))
					return err == nil
				})
				if i == 11 {
					stop()
					stop = f.start()
				}
			}
			eventually(t, "compacted journal", func() bool {
				var j struct {
					Version int                        `json:"storage_version"`
					Records map[string]json.RawMessage `json:"records"`
				}
				return p.ReadJSON(filepath.Join(f.nodeConfig.StateDir, "journal.json"), &j) == nil && len(j.Records) == 0
			})
			stop()
			files, err := filepath.Glob(filepath.Join(f.statePath+".history", "released", "*.json"))
			if err != nil || len(files) > 4 {
				t.Fatalf("history grew: %d %v", len(files), err)
			}
			if _, err := os.Stat(filepath.Join(f.statePath+".history", "released", first.ID+".json")); !os.IsNotExist(err) {
				t.Fatal("old history did not rotate")
			}
			// Restore a controller snapshot from before the first dispatch, after
			// both its result and the Node's per-task identity have been evicted.
			if err := p.WriteFile(f.statePath, beforeDispatch); err != nil {
				t.Fatal(err)
			}
			api, err := control.New(f.config, f.statePath)
			if err != nil {
				t.Fatal(err)
			}
			replaced.Store(api)
			f.admin.Close()
			f.admin = httptest.NewServer(api.AdminHandler())
			t.Cleanup(f.admin.Close)
			stop = f.start()
			f.task(first.ID, func(t p.Task) bool { return t.State == "unknown" && !t.ResultPending })
			stop()
			b, err := os.ReadFile(marker)
			if err != nil || len(b) != count {
				t.Fatalf("executions=%d want=%d err=%v", len(b), count, err)
			}
			if !lostFinal.Load() || !lostReceipt.Load() {
				t.Fatal("ack faults not exercised")
			}
		})
	}
}
