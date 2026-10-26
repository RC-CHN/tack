package node

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	p "tack.local/management/protocol"
)

func TestDiskFailureIsReportedWithoutRerunningCommand(t *testing.T) {
	cfg := testConfig(t)
	a, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	a.Logf = nil
	exe, _ := os.Executable()
	marker := filepath.Join(t.TempDir(), "once")
	task := p.Task{ID: "disk-failure", NodeID: cfg.NodeID, Sequence: 1, State: "dispatched", Command: p.Command{Mode: "exec", Program: exe, Args: []string{"-test.run=^TestHelperExecution$"}, Env: map[string]string{"TACK_TEST_HELPER": "1", "TACK_TEST_MARKER": marker, "GORACE": "atexit_sleep_ms=0"}, TimeoutSeconds: 10, OutputLimit: 100}}
	var repaired, observed, dispatched atomic.Bool
	a.writeJSON = func(path string, v any) error {
		if records, ok := v.(retainedJournal); ok && !repaired.Load() {
			if rec, ok := records.Records[task.ID]; ok && p.Terminal(rec.Report.State) {
				return errors.New("injected disk write failure")
			}
		}
		return p.WriteJSON(path, v)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a.client = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/health" {
			return response(map[string]int{"protocol_version": p.OperationsVersion}), nil
		}
		var in p.SyncRequest
		if err := p.Decode(r.Body, &in); err != nil {
			return nil, err
		}
		out := p.SyncResponse{Version: in.Version, Receipts: in.Receipts}
		if in.ReplayInit {
			zero := uint64(0)
			out.ReplayFloor = &zero
			return response(out), nil
		}
		if status := in.Node.Status; status != nil && status.Transfer != nil && status.Transfer.ErrorCode == "result_write_failed" {
			if in.AcceptTask || status.AcceptingTasks || status.BacklogReason != "result_persistence_failed" || status.Transfer.TaskID != task.ID {
				t.Error("invalid disk-pressure status")
			}
			if in.Report != nil {
				t.Error("acknowledged an unpersisted result")
			}
			observed.Store(true)
			repaired.Store(true)
		}
		if in.Report != nil && p.Terminal(in.Report.State) {
			if !observed.Load() || in.Report.State != "succeeded" {
				t.Error("lost execution outcome")
			}
			out.Acknowledged = task.ID
		}
		if len(in.Receipts) > 0 {
			cancel()
		}
		if in.AcceptTask && !dispatched.Swap(true) {
			out.Task = &task
		}
		if out.Task == nil && out.Acknowledged == "" && len(in.Receipts) == 0 && in.WaitSeconds > 0 {
			select {
			case <-r.Context().Done():
				return nil, r.Context().Err()
			case <-time.After(20 * time.Millisecond):
			}
		}
		return response(out), nil
	})}
	a.uploadClient = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		var in p.ResultChunk
		if err := p.Decode(r.Body, &in); err != nil {
			return nil, err
		}
		return response(p.ChunkAck{Version: p.TransferVersion, TaskID: in.TaskID, Complete: true}), nil
	})}
	if err := a.Run(ctx); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(marker)
	if err != nil || string(b) != "x" || !observed.Load() {
		t.Fatalf("execution/status mismatch %q %v %v", b, err, observed.Load())
	}
}
func TestV3FallbackStripsV4Metadata(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.connections.close()
	a.version = p.RetentionVersion
	a.client = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		var in map[string]any
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			return nil, err
		}
		node := in["node"].(map[string]any)
		if _, ok := node["status"]; ok {
			t.Error("v4 status leaked")
		}
		result := in["report"].(map[string]any)["result"].(map[string]any)
		for _, key := range []string{"phase", "error_code", "cleanup"} {
			if _, ok := result[key]; ok {
				t.Error("v4 field leaked", key)
			}
		}
		return response(p.SyncResponse{Version: p.RetentionVersion}), nil
	})}
	report := &p.Report{TaskID: "task", State: "failed", Result: &p.Result{ErrorCode: "command_exit_nonzero", Phase: "execute", Cleanup: &p.CleanupResult{ErrorCode: "cleanup_failed"}}}
	if _, err := a.sync(context.Background(), report, p.NodeMetrics{}); err != nil {
		t.Fatal(err)
	}
	if report.Result.Phase == "" || report.Result.Cleanup == nil {
		t.Fatal("compatibility modified durable metadata")
	}
}
func TestCleanupMetadataIsBoundedWithoutChangingOutcome(t *testing.T) {
	result := p.Result{Cleanup: &p.CleanupResult{ErrorCode: "cleanup_failed", Error: strings.Repeat("é", 5000), Path: strings.Repeat("a", 5000)}}
	boundResultMetadata(&result)
	if result.ErrorCode != "" || result.Error != "" || !result.MetadataTruncated || len(result.Cleanup.Error) > 4096 || len(result.Cleanup.Path) > 4096 {
		t.Fatal("invalid metadata bounds")
	}
}

func TestEscapedDiagnosticsFitSpoolHeader(t *testing.T) {
	result := p.Result{Error: strings.Repeat("\x00", 8192), Executable: strings.Repeat("\x00", 4096), Cleanup: &p.CleanupResult{ErrorCode: "cleanup_failed", Error: strings.Repeat("\x00", 4096), Path: strings.Repeat("\x00", 4096)}}
	boundResultMetadata(&result)
	b, _ := json.Marshal(result)
	if len(b) > 48*1024 || !result.MetadataTruncated || result.Cleanup.ErrorCode != "cleanup_failed" {
		t.Fatal("escaped metadata escaped bounds", len(b))
	}
}
