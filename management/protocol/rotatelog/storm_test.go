package rotatelog

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestConcurrentLogStormKeepsBoundedFilesAndRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "outage.log")
	w, err := Open(path, 4096, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	var workers sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		workers.Add(1)
		go func(id int) {
			defer workers.Done()
			for n := 0; n < 160; n++ {
				if _, err := fmt.Fprintf(w, "worker=%d attempt=%d transport unavailable\n", id, n); err != nil {
					t.Error(err)
					return
				}
			}
		}(worker)
	}
	workers.Wait()
	const recovery = "connection recovered\n"
	if _, err := w.Write([]byte(recovery)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) > 3 {
		t.Fatal("storm grew file count", len(entries))
	}
	var retained int64
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}
		retained += info.Size()
		if info.Size() > 4096 {
			t.Fatal("oversized log")
		}
	}
	if retained > 3*4096 {
		t.Fatal("retained bytes exceed budget", retained)
	}
	data, err := os.ReadFile(path)
	if err != nil || !bytes.HasSuffix(data, []byte(recovery)) {
		t.Fatal("recovery missing", err)
	}
	w, err = Open(path, 4096, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if _, err := w.Write([]byte("new outage\n")); err != nil {
		t.Fatal(err)
	}
	// Rotation bounds retained bytes, not aggregate writes. Do not claim
	// duplicate suppression: the production sink deliberately stores all lines.
}

func BenchmarkLogStorm(b *testing.B) {
	w, err := Open(filepath.Join(b.TempDir(), "node.log"), 64<<10, 2)
	if err != nil {
		b.Fatal(err)
	}
	defer w.Close()
	line := []byte("controller request failed: transport\n")
	b.SetBytes(int64(len(line)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := w.Write(line); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(len(line)), "logical-write-B/op")
}

func TestInvalidLogBoundsAreRejectedBeforeOpeningFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "must-not-exist")
	for _, v := range []struct {
		bytes int64
		files int
	}{{0, 1}, {1023, 1}, {1<<30 + 1, 1}, {1024, 0}, {1024, 33}} {
		if writer, err := Open(path, v.bytes, v.files); err == nil {
			writer.Close()
			t.Fatal("invalid log bounds accepted", v)
		}
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("invalid configuration created a log")
	}
}
