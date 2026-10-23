package control

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	p "tack.local/management/protocol"
)

// These private directories contain only controller history. An incomplete
// atomic archive has never produced a final acknowledgement; the Node retains
// the original result. Upload .part files have different recovery semantics and
// are deliberately not swept here.
func cleanHistoryTemps(dir string) error {
	err := walkFiles(dir, func(entry os.DirEntry) error {
		name := entry.Name()
		owned := false
		if strings.HasPrefix(name, ".state-") {
			_, err := strconv.ParseUint(strings.TrimPrefix(name, ".state-"), 10, 64)
			owned = err == nil
		}
		if strings.HasSuffix(name, ".json.tack-pending") {
			owned = p.ValidID(strings.TrimSuffix(name, ".json.tack-pending"))
		}
		if !owned {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return errors.New("invalid history temporary file")
		}
		return os.Remove(filepath.Join(dir, name))
	})
	if err != nil {
		return err
	}
	return p.SyncDirectory(dir)
}
