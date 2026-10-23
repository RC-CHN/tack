package node

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	p "tack.local/management/protocol"
)

func legacyTemp(name string) bool {
	if !strings.HasPrefix(name, ".state-") {
		return false
	}
	_, err := strconv.ParseUint(strings.TrimPrefix(name, ".state-"), 10, 64)
	return err == nil
}

// A completely written checkpoint may safely advance the fence after a crash,
// even if the corresponding command never started. Never decrease the fence.
func (a *Node) recoverReplay() error {
	path := a.replayPath()
	pending := path + ".tack-pending"
	info, err := os.Lstat(pending)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("invalid pending replay checkpoint")
	}
	var next, current replayCheckpoint
	b, err := os.ReadFile(pending)
	if err != nil {
		return err
	}
	if err := p.Decode(bytes.NewReader(b), &next); err != nil {
		if json.Valid(b) {
			return err
		}
		return os.Remove(pending)
	}
	if next.Version != 1 || next.NodeID != a.config.NodeID {
		return errors.New("mismatched pending replay checkpoint")
	}
	err = p.ReadJSON(path, &current)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err == nil && (current.NodeID != next.NodeID || current.Version != 1) {
		return errors.New("invalid replay checkpoint")
	}
	if current.High > next.High {
		return os.Remove(pending)
	}
	f, err := os.OpenFile(pending, os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	err = f.Sync()
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := p.ReplaceFile(pending, path); err != nil {
		return err
	}
	return p.SyncDirectory(a.config.StateDir)
}

// Merge recoverable task results before removing atomic-write leftovers. A
// separate recovery destination keeps the original source intact if writing
// the recovered journal itself fails. Committed acknowledgements always win.
func (a *Node) recoverJournal(records map[string]journal, retained bool) (map[string]journal, bool, error) {
	path := filepath.Join(a.config.StateDir, "journal.json")
	var cleanup []string
	flush := func() error {
		if len(cleanup) == 0 {
			return nil
		}
		var value any = records
		if retained {
			value = retainedJournal{StorageVersion: 3, Records: records}
		}
		recovery := path + ".recovery"
		if err := a.writeJSON(recovery, value); err != nil {
			return err
		}
		if err := p.ReplaceFile(recovery, path); err != nil {
			return err
		}
		if err := p.SyncDirectory(a.config.StateDir); err != nil {
			return err
		}
		for _, temp := range cleanup {
			if err := os.Remove(temp); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		cleanup = nil
		return p.SyncDirectory(a.config.StateDir)
	}
	merge := func(name string) error {
		info, e := os.Lstat(filepath.Join(a.config.StateDir, name))
		if e != nil {
			return e
		}
		if !info.Mode().IsRegular() {
			return errors.New("invalid journal temporary file")
		}
		temp := filepath.Join(a.config.StateDir, name)
		old, format, e := readJournal(temp)
		if e != nil {
			b, readErr := os.ReadFile(temp)
			if readErr != nil {
				return readErr
			}
			if json.Valid(b) {
				var identity struct {
					NodeID     string `json:"node_id"`
					InstanceID string `json:"instance_id,omitempty"`
				}
				if !legacyTemp(name) || p.ReadJSON(temp, &identity) != nil || identity.NodeID != a.config.NodeID {
					return e
				}
			}
		}
		if e == nil && old != nil {
			for id, rec := range old {
				if !p.ValidID(id) || rec.Task.ID != id || rec.Report.TaskID != id || rec.Task.NodeID != a.config.NodeID {
					return errors.New("temporary journal identity mismatch")
				}
				if p.Terminal(rec.Report.State) && rec.Report.Result == nil {
					return errors.New("temporary terminal result is invalid")
				}
				cur, ok := records[id]
				if ok && !cur.Acknowledged && p.Terminal(cur.Report.State) && p.Terminal(rec.Report.State) && cur.Report.Result != nil {
					if cur.Report.State != rec.Report.State || p.OutputManifest(cur.Report.Result) != p.OutputManifest(rec.Report.Result) {
						return errors.New("conflicting unconfirmed recovery results; preserving both")
					}
				}
				if !ok || (!p.Terminal(cur.Report.State) && p.Terminal(rec.Report.State)) {
					records[id] = rec
				}
			}
			retained = retained || format
		}
		cleanup = append(cleanup, temp)
		return nil
	}
	// Read every recovery destination before writing to one. Otherwise a
	// legacy directory batch could overwrite an unread, complete recovery file.
	for _, name := range []string{"journal.json.recovery", "journal.json.recovery.tack-pending", "journal.json.tack-pending"} {
		if _, err := os.Lstat(filepath.Join(a.config.StateDir, name)); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return nil, retained, err
		}
		if err := merge(name); err != nil {
			return nil, retained, err
		}
	}
	if err := flush(); err != nil {
		return nil, retained, err
	}
	f, err := os.Open(a.config.StateDir)
	if err != nil {
		return nil, retained, err
	}
	defer f.Close()
	for {
		entries, err := f.ReadDir(128)
		for _, entry := range entries {
			if !legacyTemp(entry.Name()) {
				continue
			}
			if err := merge(entry.Name()); err != nil {
				return nil, retained, err
			}
		}
		if e := flush(); e != nil {
			return nil, retained, e
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, retained, err
		}
	}
	return records, retained, nil
}
