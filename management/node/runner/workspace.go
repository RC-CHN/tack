package runner

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	p "tack.local/management/protocol"
)

// Owner is the persistent node instance ID. The caller holds its state lock and
// runs one command at a time. Customer-selected paths are never swept directly.
func (r Runner) Recover() (string, error) {
	if r.Owner == "" {
		return r.WorkDir, nil
	} // Standalone runner callers manage their own workspaces.
	if !p.ValidID(r.Owner) {
		return "", errors.New("invalid workspace owner")
	}
	dir := filepath.Join(r.WorkDir, "tack-"+r.Owner)
	if err := os.Mkdir(dir, 0700); err != nil && !os.IsExist(err) {
		return "", err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("invalid owned workspace directory")
	}
	f, err := os.Open(dir)
	if err != nil {
		return "", err
	}
	defer f.Close()
	for {
		entries, err := f.ReadDir(128)
		for _, entry := range entries {
			if !strings.HasPrefix(entry.Name(), "task-") {
				return "", errors.New("unrecognized file in owned workspace")
			}
			if err := os.RemoveAll(filepath.Join(dir, entry.Name())); err != nil {
				return "", err
			}
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return "", err
		}
	}
	if err := p.SyncDirectory(dir); err != nil {
		return "", err
	}
	return dir, nil
}
