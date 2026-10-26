package control

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	p "tack.local/management/protocol"
)

func copySequences(in map[string]uint64) map[string]uint64 {
	out := make(map[string]uint64, len(in))
	for id, n := range in {
		out[id] = n
	}
	return out
}

func validateReceipts(in p.SyncRequest, version int) error {
	if version < p.RetentionVersion && (in.ReplayInit || in.ReplayHigh != 0 || len(in.Receipts) != 0) {
		return errors.New("retention requires protocol v3")
	}
	if len(in.Receipts) > 128 || (in.ReplayInit && (in.Report != nil || len(in.Receipts) != 0 || in.AcceptTask)) {
		return errors.New("invalid replay initialization or receipt batch")
	}
	seen := map[string]bool{}
	for _, id := range in.Receipts {
		if !p.ValidID(id) || seen[id] {
			return errors.New("invalid receipt identity")
		}
		seen[id] = true
	}
	return nil
}

// Caller holds tx and mu. Only publish the counter after durable replacement.
func (s *Server) advanceReplay(id string, high uint64) error {
	if high <= s.state.Sequences[id] {
		return nil
	}
	next := s.state
	next.Sequences = copySequences(next.Sequences)
	next.Sequences[id] = high
	s.mu.Unlock()
	err := s.writeJSON(s.path, next)
	s.mu.Lock()
	if err == nil {
		s.state = next
	}
	return err
}

func (s *Server) walkHistory(fn func(string, os.DirEntry) error) error {
	for _, dir := range []string{s.path + ".history", filepath.Dir(s.releasedPath("unused"))} {
		if err := walkFiles(dir, func(e os.DirEntry) error { return fn(dir, e) }); err != nil {
			return err
		}
	}
	return nil
}

// The released directory is the durable receipt index. Moving a completed
// result here is permitted only after the Node confirms receiving the final ack.
// It avoids retaining a per-task index in RAM or reading every output to evict.
func (s *Server) releaseReceipts(node string, ids []string) error {
	for _, id := range ids {
		s.mu.Lock()
		_, active := s.state.Tasks[id]
		s.mu.Unlock()
		if active {
			return errors.New("receipt for active task")
		}
		owner, err := s.receiptOwner(id)
		if err != nil {
			return err
		}
		if owner == "" {
			continue
		} // Node already durably received the final ack, even after a controller restore.
		if owner != node {
			return errors.New("receipt does not belong to completed node task")
		}
		// The final upload ack requires successful staging cleanup. Startup also
		// cleans completed staging files before this endpoint can be used.
		err = p.ReplaceFile(s.historyPath(id), s.releasedPath(id))
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if err == nil && s.evictions.valid {
			info, err := os.Stat(s.releasedPath(id))
			if err != nil {
				return err
			}
			s.evictions.add(eviction{id: id, size: info.Size(), modified: info.ModTime()})
		}
		if err := p.SyncDirectory(s.path + ".history"); err != nil {
			return err
		}
		if err := p.SyncDirectory(filepath.Dir(s.releasedPath(id))); err != nil {
			return err
		}
	}
	return nil
}

type eviction struct {
	id       string
	size     int64
	modified time.Time
}

func (s *Server) moveReleased(id string) error {
	if _, err := os.Stat(s.releasedPath(id)); err == nil {
		return errors.New("duplicate history during migration")
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := p.ReplaceFile(s.historyPath(id), s.releasedPath(id)); err != nil {
		return err
	}
	s.evictions.valid = false
	if err := p.SyncDirectory(s.path + ".history"); err != nil {
		return err
	}
	return p.SyncDirectory(filepath.Dir(s.releasedPath(id)))
}

// A one-time upgrade scan handles old cancellations that were never sent to a
// Node and therefore can never receive a receipt. Decode one bounded archive
// at a time, before persisting storage version 2; interrupted upgrades retry.
func (s *Server) releaseLegacyCancellations() error {
	return walkFiles(s.path+".history", func(e os.DirEntry) error {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			return nil
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		t, ok, err := s.readHistory(id)
		if err != nil || !ok {
			return err
		}
		if t.State == "canceled" && (t.Timing == nil || t.Timing.DispatchedAt.IsZero()) {
			return s.moveReleased(id)
		}
		return nil
	})
}

func (s *Server) receiptOwner(id string) (string, error) {
	f, err := os.Open(s.historyPath(id))
	if os.IsNotExist(err) {
		f, err = os.Open(s.releasedPath(id))
	}
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer f.Close()
	// Both archive formats written by this controller begin with ID, optional
	// sequence, then node_id. Read only that bounded header: a receipt must not
	// allocate/decode megabytes of already-confirmed output. Only completed
	// records enter history; the caller also rejects any still-active task.
	d := json.NewDecoder(io.LimitReader(f, 4096))
	start, err := d.Token()
	if err != nil || start != json.Delim('{') {
		return "", errors.New("invalid history header")
	}
	field, err := d.Token()
	var savedID string
	if err != nil || field != "id" || d.Decode(&savedID) != nil || savedID != id {
		return "", errors.New("invalid history identity")
	}
	field, err = d.Token()
	if err == nil && field == "sequence" {
		var seq uint64
		if err := d.Decode(&seq); err != nil {
			return "", err
		}
		field, err = d.Token()
	}
	var owner string
	if err != nil || field != "node_id" || d.Decode(&owner) != nil || !p.ValidID(owner) {
		return "", errors.New("invalid history owner")
	}
	return owner, nil
}

const evictionBatch = 128

type evictionCache struct {
	items           []eviction
	valid, complete bool
	boundary        eviction // New arrivals beyond an incomplete batch wait for its refill.
	scans           uint64
}

func earlier(a, b eviction) bool {
	return a.modified.Before(b.modified) || (a.modified.Equal(b.modified) && a.id < b.id)
}

func (c *evictionCache) add(v eviction) {
	if !c.complete && earlier(c.boundary, v) {
		return
	}
	i := sort.Search(len(c.items), func(i int) bool { return !earlier(c.items[i], v) })
	if i == evictionBatch {
		c.complete = false
		return
	}
	if len(c.items) == evictionBatch {
		copy(c.items[i+1:], c.items[i:len(c.items)-1])
		c.items[i] = v
		c.complete = false
	} else {
		c.items = append(c.items, eviction{})
		copy(c.items[i+1:], c.items[i:])
		c.items[i] = v
	}
	if len(c.items) == evictionBatch {
		c.boundary = c.items[len(c.items)-1]
	}
}

func (s *Server) fillEvictions(active map[string]bool) error {
	c := &s.evictions
	if c.items == nil {
		c.items = make([]eviction, 0, evictionBatch)
	}
	c.items = c.items[:0]
	c.complete = true
	c.valid = false
	c.scans++
	err := walkFiles(filepath.Dir(s.releasedPath("unused")), func(e os.DirEntry) error {
		if !strings.HasSuffix(e.Name(), ".json") || e.IsDir() {
			return nil
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		if !p.ValidID(id) || active[id] {
			return nil
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return errors.New("invalid released history entry")
		}
		c.add(eviction{id: id, size: info.Size(), modified: info.ModTime()})
		return nil
	})
	c.valid = err == nil
	return err
}

// tx is held. Cache only the oldest 128 metadata records, regardless of total
// history size. Admissions reuse the batch; each minute Maintain refreshes it.
// Pending uploads and records awaiting the final ack never enter this directory.
func (s *Server) pruneHistory(extra *p.Task) (err error) {
	s.mu.Lock()
	active := make(map[string]bool, len(s.state.Tasks))
	var reserved int64
	for id, t := range s.state.Tasks {
		active[id] = true
		reserved += reservation(t)
	}
	s.mu.Unlock()
	count := len(active) + s.orphanFiles
	reserved += s.orphanBytes
	if extra != nil {
		if reservation(*extra) > s.config.MaxHistoryBytes {
			return errors.New("task exceeds history byte budget")
		}
		count++
		reserved += reservation(*extra)
	}
	pressure := func() bool {
		return s.historyTasks+count > s.config.MaxHistoryTasks || s.historyBytes+reserved > s.config.MaxHistoryBytes
	}
	cutoff := time.Now().Add(-time.Duration(s.config.HistoryRetentionSeconds) * time.Second)
	removed := false
	defer func() {
		if removed {
			syncErr := p.SyncDirectory(filepath.Dir(s.releasedPath("unused")))
			if err == nil {
				err = syncErr
			}
		}
	}()
	c := &s.evictions
	for {
		if !c.valid || (len(c.items) == 0 && !c.complete) {
			if err := s.fillEvictions(active); err != nil {
				return err
			}
		}
		if len(c.items) == 0 {
			return nil
		}
		v := c.items[0]
		if !pressure() && !v.modified.Before(cutoff) {
			return nil
		}
		if !active[v.id] {
			if err := s.retireRequest(v.id); err != nil {
				return err
			}
			if err := os.Remove(s.releasedPath(v.id)); err != nil {
				c.valid = false
				return err
			}
			s.historyTasks--
			s.historyBytes -= v.size
			removed = true
		}
		copy(c.items, c.items[1:])
		c.items = c.items[:len(c.items)-1]
	}
}

// Maintain is called by the foreground controller's bounded periodic worker.
// Tests may call it explicitly without starting a background goroutine.
func (s *Server) Maintain() error {
	s.tx.Lock()
	defer s.tx.Unlock()
	s.evictions.valid = false
	if err := s.expireQueued(time.Now()); err != nil {
		return err
	}
	return s.pruneHistory(nil)
}
