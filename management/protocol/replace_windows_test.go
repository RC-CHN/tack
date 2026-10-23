package protocol

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStateReplacementSurvivesBriefWindowsReader(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := WriteFile(path, []byte("original")); err != nil {
		t.Fatal(err)
	}
	// Go opens regular files without FILE_SHARE_DELETE on Windows.
	reader, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	done := make(chan error, 1)
	go func() { done <- WriteFile(path, []byte("replacement")) }()
	select {
	case err := <-done:
		t.Fatalf("replacement completed while destination was held: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("replacement did not recover after reader closed")
	}
	b, err := os.ReadFile(path)
	if err != nil || string(b) != "replacement" {
		t.Fatal(string(b), err)
	}
	if _, err := os.Stat(path + ".tack-pending"); !os.IsNotExist(err) {
		t.Fatal("pending file retained", err)
	}
}

func TestStateReplacementPermanentWindowsConflictIsBounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := WriteFile(path, []byte("original")); err != nil {
		t.Fatal(err)
	}
	reader, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	start := time.Now()
	if err := WriteFile(path, []byte("replacement")); err == nil {
		t.Fatal("acknowledged uncommitted replacement")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("unbounded replacement retry")
	}
	b, err := os.ReadFile(path)
	if err != nil || string(b) != "original" {
		t.Fatal("lost original state", err)
	}
	if _, err := os.Stat(path + ".tack-pending"); !os.IsNotExist(err) {
		t.Fatal("pending file retained", err)
	}
}
