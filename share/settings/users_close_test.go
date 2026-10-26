package settings

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jpillora/chisel/share/cio"
)

func TestCloseWaitsForAuthWatcher(t *testing.T) {
	path := filepath.Join(t.TempDir(), "users.json")
	if err := os.WriteFile(path, []byte(`{"node:password":["^socks$"]}`), 0600); err != nil {
		t.Fatal(err)
	}
	users := NewUserIndex(cio.NewLogger("test"))
	if err := users.LoadUsers(path); err != nil {
		t.Fatal(err)
	}
	if err := users.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-users.watchDone:
	default:
		t.Fatal("Close returned with watcher still running")
	}
	if err := users.Close(); err != nil {
		t.Fatal(err)
	}
}
