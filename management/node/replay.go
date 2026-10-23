package node

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	p "tack.local/management/protocol"
)

type retainedJournal struct {
	StorageVersion int                `json:"storage_version"`
	Records        map[string]journal `json:"records"`
}

func readJournal(path string) (map[string]journal, bool, error) {
	return visitJournal(path, nil)
}

// Inspect the envelope without buffering its result-bearing records. Legacy
// files and differently ordered JSON envelopes remain readable.
func skipJSON(d *json.Decoder) error {
	token, err := d.Token()
	if err != nil {
		return err
	}
	if delim, ok := token.(json.Delim); ok && (delim == '{' || delim == '[') {
		for d.More() {
			if err := skipJSON(d); err != nil {
				return err
			}
		}
		_, err = d.Token()
	}
	return err
}

func journalEnvelope(f *os.File) (bool, error) {
	d := json.NewDecoder(f)
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return false, errors.New("invalid journal object")
	}
	for d.More() {
		key, err := d.Token()
		if err != nil {
			return false, err
		}
		if key == "storage_version" {
			var raw json.RawMessage
			if err := d.Decode(&raw); err != nil {
				return false, err
			}
			var version int
			if json.Unmarshal(raw, &version) == nil {
				if version != 3 && version != 4 {
					return true, errors.New("unsupported journal storage version")
				}
				return true, nil
			}
		} else if err := skipJSON(d); err != nil {
			return false, err
		}
	}
	return false, nil
}

func visitJournal(path string, convert func(journal) (journal, error)) (map[string]journal, bool, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return map[string]journal{}, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	retained, err := journalEnvelope(f)
	if err != nil {
		return nil, retained, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, retained, err
	}
	d := json.NewDecoder(f)
	d.DisallowUnknownFields()
	records := map[string]journal{}
	readMap := func() error {
		if token, err := d.Token(); err != nil || token != json.Delim('{') {
			return errors.New("invalid journal records")
		}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return err
			}
			id, ok := key.(string)
			if !ok || !p.ValidID(id) {
				return errors.New("invalid journal identity")
			}
			if _, ok := records[id]; ok {
				return errors.New("duplicate journal identity")
			}
			if len(records) >= 10000 {
				return errors.New("journal record limit exceeded")
			}
			var rec journal
			if err := d.Decode(&rec); err != nil {
				return err
			}
			if rec.Task.ID != id || rec.Report.TaskID != id {
				return errors.New("mismatched journal identity")
			}
			if rec.Spool != nil {
				if !retained || !p.Terminal(rec.Report.State) || rec.Report.Result == nil || len(rec.Report.Result.Stdout)+len(rec.Report.Result.Stderr) != 0 {
					return errors.New("invalid disk-backed journal result")
				}
				if !rec.Acknowledged && rec.Spool.Validate(p.MaxOutput) != nil {
					return errors.New("invalid journal output manifest")
				}
			}
			if retained && convert != nil {
				rec, err = convert(rec)
				if err != nil {
					return err
				}
			}
			records[id] = rec
		}
		_, err := d.Token()
		return err
	}
	if retained {
		if token, err := d.Token(); err != nil || token != json.Delim('{') {
			return nil, true, errors.New("invalid journal envelope")
		}
		seen := map[string]bool{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return nil, true, err
			}
			field, _ := key.(string)
			if seen[field] {
				return nil, true, errors.New("duplicate journal field")
			}
			seen[field] = true
			switch field {
			case "storage_version":
				var version int
				if err := d.Decode(&version); err != nil || (version != 3 && version != 4) {
					return nil, true, errors.New("invalid journal version")
				}
			case "records":
				if err := readMap(); err != nil {
					return nil, true, err
				}
			default:
				return nil, true, errors.New("unknown journal field")
			}
		}
		if !seen["records"] || !seen["storage_version"] {
			return nil, true, errors.New("incomplete journal envelope")
		}
		if _, err := d.Token(); err != nil {
			return nil, true, err
		}
	} else if err := readMap(); err != nil {
		return nil, false, err
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, retained, errors.New("unexpected trailing journal data")
	}
	return records, retained, nil
}

// The high watermark is flushed BEFORE execution intent and is never rolled
// back when individual acknowledged records are removed. A crash between the
// two writes may refuse an unstarted command, but can never execute it twice.
type replayCheckpoint struct {
	Version int    `json:"version"`
	NodeID  string `json:"node_id"`
	High    uint64 `json:"high"`
}

func (a *Node) replayPath() string { return filepath.Join(a.config.StateDir, "replay.json") }

func (a *Node) openReplay(ctx context.Context, records map[string]journal) (uint64, error) {
	var cp replayCheckpoint
	err := p.ReadJSON(a.replayPath(), &cp)
	if err == nil {
		if cp.Version != 1 || cp.NodeID != a.config.NodeID || a.version < p.LatestVersion {
			return 0, errors.New("replay checkpoint requires this node identity and protocol v3; unsafe downgrade refused")
		}
		for _, rec := range records {
			if rec.Task.Sequence > cp.High {
				return 0, errors.New("journal exceeds durable replay watermark")
			}
		}
		return cp.High, nil
	}
	if !os.IsNotExist(err) {
		return 0, err
	}
	if a.version < p.LatestVersion {
		return 0, nil
	}
	var high uint64
	if len(records) != 0 {
		// Old nodes did not retain sequence numbers. Fence every task already
		// allocated by the controller before discarding any legacy tombstone.
		in := p.SyncRequest{Version: p.LatestVersion, InstanceID: a.instance, Node: p.Node{ID: a.config.NodeID}, ReplayInit: true, LeaseSeconds: a.config.LeaseSeconds}
		var out p.SyncResponse
		if err := a.request(ctx, a.client, p.NodeRetentionPath, in, &out); err != nil {
			return 0, err
		}
		if out.Version != p.LatestVersion || out.ReplayFloor == nil || out.Task != nil {
			return 0, errors.New("invalid replay migration fence")
		}
		high = *out.ReplayFloor
	}
	return high, a.saveReplay(high)
}

func (a *Node) saveReplay(high uint64) error {
	return a.writeJSON(a.replayPath(), replayCheckpoint{Version: 1, NodeID: a.config.NodeID, High: high})
}

func receiptBatch(records map[string]journal) []string {
	var ids []string
	for id, rec := range records {
		if rec.Acknowledged {
			ids = append(ids, id)
			if len(ids) == 128 {
				break
			}
		}
	}
	return ids
}

func hasReceipts(records map[string]journal) bool {
	for _, rec := range records {
		if rec.Acknowledged {
			return true
		}
	}
	return false
}
