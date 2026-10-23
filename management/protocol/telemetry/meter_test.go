package telemetry

import (
	"path/filepath"
	"testing"
	"time"
)

func TestFailuresAndStateAccounting(t *testing.T) {
	m := New()
	m.Request(false, time.Now().Add(-20*time.Millisecond), 503, "http")
	m.Request(false, time.Now(), 200, "")
	m.Request(true, time.Now(), 0, "timeout")
	m.Request(false, time.Now(), 0, "canceled")
	path := filepath.Join(t.TempDir(), "state.json")
	if err := m.WriteJSON(path, map[string]string{"key": "value"}); err != nil {
		t.Fatal(err)
	}
	if err := m.WriteJSON(filepath.Join(path, "impossible.json"), "x"); err == nil {
		t.Fatal("expected write error")
	}
	v := m.Snapshot()
	if v.Control.Requests != 3 || v.Control.Canceled != 1 || v.Control.Failures != 1 || v.Control.ConsecutiveFailures != 0 || v.Control.MaxDurationMS < 20 || v.Upload.Failures != 1 || v.StateWrites != 2 || v.StateWriteFailures != 1 || v.StateBytesWritten == 0 || v.GoAllocBytes == 0 || v.Goroutines == 0 {
		t.Fatalf("%+v", v)
	}
}
