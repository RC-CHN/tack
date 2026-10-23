package rotatelog

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestRotationBoundsAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node.log")
	w, err := Open(path, 1024, 3)
	if err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 25000)
	for i := range data {
		data[i] = byte(i % 251)
	}
	for start := 0; start < len(data); {
		end := start + 317
		if end > len(data) {
			end = len(data)
		}
		if _, err := w.Write(data[start:end]); err != nil {
			t.Fatal(err)
		}
		start = end
	}
	w.Close()
	var retained []byte
	for i := 2; i >= 0; i-- {
		name := path
		if i > 0 {
			name += "." + strconv.Itoa(i)
		}
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if len(b) > 1024 {
			t.Fatal("oversized log")
		}
		retained = append(retained, b...)
	}
	if !bytes.Equal(retained, data[len(data)-len(retained):]) {
		t.Fatal("rotation lost newest log bytes")
	}
	w, err = Open(path, 1024, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(bytes.Repeat([]byte("z"), 100000)); err != nil {
		t.Fatal(err)
	}
	w.Close()
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 2 {
		t.Fatalf("restart/config shrink retained %d files", len(entries))
	}
	for _, entry := range entries {
		info, _ := entry.Info()
		if info.Size() > 1024 {
			t.Fatal("oversized rotated file")
		}
	}
}
