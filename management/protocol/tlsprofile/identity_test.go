package tlsprofile

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

func TestIdentityConcurrentCreationAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity.json")
	var wg sync.WaitGroup
	identities := make([]*Identity, 16)
	for i := range identities {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var err error
			identities[i], err = LoadIdentity(path)
			if err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	for _, identity := range identities {
		if identity == nil || identities[0] == nil || identity.seed != identities[0].seed {
			t.Fatal("concurrent creators disagreed")
		}
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && before.Mode().Perm() != 0600 {
		t.Fatal("identity permissions", before.Mode())
	}
	data, _ := os.ReadFile(path)
	reloaded, err := LoadIdentity(path)
	if err != nil || reloaded.seed != identities[0].seed {
		t.Fatal("restart changed identity", err)
	}
	after, _ := os.Stat(path)
	if !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("read rewrote identity")
	}
	other, err := LoadIdentity(filepath.Join(filepath.Dir(path), "other.json"))
	if err != nil || other.seed == reloaded.seed {
		t.Fatal("clients reused seed", err)
	}
	current, _ := os.ReadFile(path)
	if !bytes.Equal(data, current) {
		t.Fatal("other client changed identity")
	}
	leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".tls-identity-*"))
	if len(leftovers) != 0 {
		t.Fatal("temporary file leak", leftovers)
	}
}

func TestIdentityInvalidFileNeverRotates(t *testing.T) {
	for _, data := range []string{`{`, `null`, `{"version":2,"seed":"00"}`, `{"version":1,"seed":"not hex"}`, `{"version":1,"seed":"00","unknown":true}`, string(bytes.Repeat([]byte{'x'}, 1025))} {
		path := filepath.Join(t.TempDir(), "identity.json")
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadIdentity(path); err == nil {
			t.Fatal("accepted invalid identity")
		}
		current, _ := os.ReadFile(path)
		if string(current) != data {
			t.Fatal("silently regenerated invalid identity")
		}
	}
	if _, err := LoadIdentity("relative.json"); err == nil {
		t.Fatal("accepted relative identity path")
	}
	dir := t.TempDir()
	if _, err := LoadIdentity(dir); err == nil {
		t.Fatal("accepted directory")
	}
	path := filepath.Join(dir, "real.json")
	if _, err := LoadIdentity(path); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "symlink.json")
	if err := os.Symlink(path, link); err == nil {
		if _, err := LoadIdentity(link); err == nil {
			t.Fatal("followed identity symlink")
		}
	}
}
