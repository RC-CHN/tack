package node

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	p "tack.local/management/protocol"
)

type retainedJournal struct {
	StorageVersion int                `json:"storage_version"`
	Records        map[string]journal `json:"records"`
}

func readJournal(path string) (map[string]journal, bool, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return map[string]journal{}, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(b, &fields); err != nil {
		return nil, false, err
	}
	var version int
	if raw, ok := fields["storage_version"]; ok && json.Unmarshal(raw, &version) == nil {
		var in retainedJournal
		if err := p.Decode(bytes.NewReader(b), &in); err != nil {
			return nil, true, err
		}
		if in.StorageVersion != 3 || in.Records == nil {
			return nil, true, errors.New("invalid retained journal")
		}
		return in.Records, true, nil
	}
	var records map[string]journal
	err = p.Decode(bytes.NewReader(b), &records)
	return records, false, err
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
