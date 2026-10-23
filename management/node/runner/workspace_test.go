package runner

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRecoverOnlyOwnedWorkspace(t *testing.T) {
	base := t.TempDir()
	r := Runner{WorkDir: base, Owner: "node-instance"}
	root, err := r.Recover()
	if err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(root, "task-old")
	os.Mkdir(old, 0700)
	os.WriteFile(filepath.Join(old, "script.sh"), []byte("leftover"), 0600)
	other := filepath.Join(base, "task-customer")
	os.Mkdir(other, 0700)
	os.WriteFile(filepath.Join(other, "keep"), []byte("customer"), 0600)
	if _, err := r.Recover(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("owned crash workspace retained")
	}
	if _, err := os.Stat(filepath.Join(other, "keep")); err != nil {
		t.Fatal("customer workspace touched")
	}
	os.WriteFile(filepath.Join(root, "unexpected"), []byte("preserve"), 0600)
	if _, err := r.Recover(); err == nil {
		t.Fatal("unrecognized content was silently swept")
	}
}
