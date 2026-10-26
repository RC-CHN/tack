package node

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	p "tack.local/management/protocol"
)

// A result is immutable: bounded JSON metadata followed by raw stdout/stderr.
// Only the small journal references it. Its own metadata can recover completion
// if the process stops between committing the body and updating the journal.
const spoolMagic = "TACKOUT1"
const maxSpoolHeader = 64 * 1024

var errIncompleteSpool = errors.New("incomplete result file")

type spoolHeader struct {
	NodeID   string     `json:"node_id"`
	TaskID   string     `json:"task_id"`
	Sequence uint64     `json:"sequence"`
	State    string     `json:"state"`
	Result   p.Result   `json:"result"`
	Manifest p.Manifest `json:"manifest"`
}

func journalManifest(rec journal) p.Manifest {
	if rec.Spool != nil {
		return *rec.Spool
	}
	return p.OutputManifest(rec.Report.Result)
}

func (a *Node) spoolDir() string           { return filepath.Join(a.config.StateDir, "results") }
func (a *Node) spoolPath(id string) string { return filepath.Join(a.spoolDir(), id+".out") }

func (a *Node) ensureSpoolDir() error {
	if err := os.Mkdir(a.spoolDir(), 0700); err != nil && !os.IsExist(err) {
		return err
	}
	info, err := os.Lstat(a.spoolDir())
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("result storage must be a directory, not a symlink")
	}
	return p.SyncDirectory(a.config.StateDir)
}

// readSpool holds at most a header and a 32 KiB hashing buffer. It never loads
// the body into memory. The returned file is positioned at the raw output.
func readSpool(path string, verify bool) (*os.File, spoolHeader, int64, error) {
	var h spoolHeader
	info, err := os.Lstat(path)
	if err != nil {
		return nil, h, 0, err
	}
	if !info.Mode().IsRegular() {
		return nil, h, 0, errors.New("result file must be regular")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, h, 0, err
	}
	fail := func(err error) (*os.File, spoolHeader, int64, error) { f.Close(); return nil, h, 0, err }
	var prefix [12]byte
	if _, err := io.ReadFull(f, prefix[:]); err != nil {
		return fail(errIncompleteSpool)
	}
	n := binary.BigEndian.Uint32(prefix[8:])
	if string(prefix[:8]) != spoolMagic || n == 0 || n > maxSpoolHeader {
		return fail(errors.New("invalid result header"))
	}
	b := make([]byte, int(n))
	if _, err := io.ReadFull(f, b); err != nil {
		return fail(errIncompleteSpool)
	}
	if err := p.Decode(bytes.NewReader(b), &h); err != nil {
		return fail(err)
	}
	if !p.ValidID(h.TaskID) || !p.ValidID(h.NodeID) || !p.Terminal(h.State) || len(h.Result.Stdout) != 0 || len(h.Result.Stderr) != 0 || h.Manifest.Validate(p.MaxOutput) != nil {
		return fail(errors.New("invalid result metadata"))
	}
	offset := int64(len(prefix)) + int64(n)
	want := offset + int64(h.Manifest.Total())
	if info.Size() < want {
		return fail(errIncompleteSpool)
	}
	if info.Size() != want {
		return fail(errors.New("unexpected result file length"))
	}
	if verify {
		digest := sha256.New()
		if _, err := io.CopyN(digest, f, int64(h.Manifest.Total())); err != nil {
			return fail(err)
		}
		if hex.EncodeToString(digest.Sum(nil)) != h.Manifest.SHA256 {
			return fail(errors.New("result file digest mismatch; preserved for recovery"))
		}
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return fail(err)
		}
	}
	return f, h, offset, nil
}

func equalSpool(a, b spoolHeader) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

func (a *Node) storeSpool(h spoolHeader, stdout, stderr []byte) error {
	if err := a.ensureSpoolDir(); err != nil {
		return err
	}
	path := a.spoolPath(h.TaskID)
	// A retry never replaces an existing, conflicting result.
	if f, saved, _, err := readSpool(path, true); err == nil {
		f.Close()
		if !equalSpool(h, saved) {
			return errors.New("conflicting result file; preserving original")
		}
		return p.SyncDirectory(a.spoolDir())
	} else if !os.IsNotExist(err) {
		return err
	}
	pending := path + ".tack-pending"
	if f, saved, _, err := readSpool(pending, true); err == nil {
		f.Close()
		if !equalSpool(h, saved) {
			return errors.New("conflicting pending result; preserving original")
		}
		return a.commitSpool(pending, path)
	} else if errors.Is(err, errIncompleteSpool) {
		// The caller still owns the complete output. An incomplete prior write
		// cannot add evidence, and must not block recovery of the committed
		// legacy journal during a streaming migration.
		if err := os.Remove(pending); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	header, err := json.Marshal(h)
	if err != nil {
		return err
	}
	if len(header) > maxSpoolHeader {
		return errors.New("result metadata exceeds storage limit")
	}
	f, err := os.OpenFile(pending, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	complete := false
	defer func() {
		f.Close()
		if !complete {
			_ = os.Remove(pending)
		}
	}()
	var prefix [12]byte
	copy(prefix[:8], spoolMagic)
	binary.BigEndian.PutUint32(prefix[8:], uint32(len(header)))
	for _, part := range [][]byte{prefix[:], header, stdout, stderr} {
		if n, err := f.Write(part); err != nil {
			return err
		} else if n != len(part) {
			return io.ErrShortWrite
		}
	}
	complete = true // Preserve complete data if sync/rename fails, for retry/recovery.
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := p.ReplaceFile(pending, path); err != nil {
		return err
	}
	return p.SyncDirectory(a.spoolDir())
}

func (a *Node) commitSpool(pending, path string) error {
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
	return p.SyncDirectory(a.spoolDir())
}

func compactTask(t p.Task) p.Task {
	return p.Task{ID: t.ID, NodeID: t.NodeID, Sequence: t.Sequence, CreatedAt: t.CreatedAt}
}

func (a *Node) persistJournal(path string, records map[string]journal, retained bool) error {
	if !retained {
		return a.writeJSON(path, records)
	}
	for id, rec := range records {
		next, err := a.spoolRecord(rec)
		if err != nil {
			return err
		}
		// Execution intent needs identity and sequence, not a second persisted
		// copy of a potentially large command. The runner owns its command.
		next.Task = compactTask(next.Task)
		records[id] = next
	}
	if err := a.writeJSON(path, retainedJournal{StorageVersion: 4, Records: records}); err != nil {
		return err
	}
	return a.cleanAcknowledgedSpools(records)
}

func (a *Node) spoolRecord(rec journal) (journal, error) {
	if !p.Terminal(rec.Report.State) || rec.Acknowledged || rec.Spool != nil || rec.Report.Result == nil {
		return rec, nil
	}
	result := *rec.Report.Result
	boundResultMetadata(&result)
	if len(result.Stdout)+len(result.Stderr) == 0 {
		rec.Task = compactTask(rec.Task)
		rec.Report.Result = &result
		return rec, nil
	}
	manifest := p.OutputManifest(&result)
	if err := manifest.Validate(p.MaxOutput); err != nil {
		return rec, err
	}
	stdout, stderr := result.Stdout, result.Stderr
	result.Stdout, result.Stderr = nil, nil
	h := spoolHeader{NodeID: rec.Task.NodeID, TaskID: rec.Task.ID, Sequence: rec.Task.Sequence, State: rec.Report.State, Result: result, Manifest: manifest}
	if err := a.storeSpool(h, stdout, stderr); err != nil {
		return rec, err
	}
	rec.Task, rec.Report.Result, rec.Spool = compactTask(rec.Task), &result, &manifest
	return rec, nil
}

// Called ONLY after the acknowledgement-bearing journal has been flushed.
// Receipt exchange/removal cannot start until this cleanup succeeds.
func (a *Node) cleanAcknowledgedSpools(records map[string]journal) error {
	for id, rec := range records {
		if !rec.Acknowledged || rec.Spool == nil {
			continue
		}
		for _, path := range []string{a.spoolPath(id), a.spoolPath(id) + ".tack-pending"} {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		if err := p.SyncDirectory(a.spoolDir()); err != nil && !os.IsNotExist(err) {
			return err
		}
		rec.Spool = nil
		records[id] = rec
	}
	return nil
}

func (a *Node) recoverSpools(records map[string]journal) error {
	if info, err := os.Lstat(a.spoolDir()); err == nil && !info.IsDir() {
		return errors.New("result storage must be a directory, not a symlink")
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	f, err := os.Open(a.spoolDir())
	if os.IsNotExist(err) {
		for _, rec := range records {
			if rec.Spool != nil && !rec.Acknowledged {
				return errors.New("missing result storage")
			}
		}
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	for {
		entries, err := f.ReadDir(128)
		for _, entry := range entries {
			name := entry.Name()
			pending := strings.HasSuffix(name, ".out.tack-pending")
			id := strings.TrimSuffix(strings.TrimSuffix(name, ".tack-pending"), ".out")
			if !p.ValidID(id) || (!pending && name != id+".out") || entry.IsDir() {
				return errors.New("unrecognized result storage; preserving for recovery")
			}
			rec, ok := records[id]
			if !ok {
				return fmt.Errorf("orphan result %s; preserving unconfirmed data", id)
			}
			if rec.Acknowledged {
				// The durable receipt authorizes cleanup even if the body is partial.
				if rec.Spool == nil {
					rec.Spool = &p.Manifest{}
					records[id] = rec
				}
				continue
			}
			path := filepath.Join(a.spoolDir(), name)
			body, header, _, e := readSpool(path, true)
			if pending && errors.Is(e, errIncompleteSpool) {
				if e := os.Remove(path); e != nil {
					return e
				}
				if e := p.SyncDirectory(a.spoolDir()); e != nil {
					return e
				}
				continue
			}
			if e != nil {
				return e
			}
			body.Close()
			if header.NodeID != a.config.NodeID || header.TaskID != id || header.Sequence != rec.Task.Sequence {
				return errors.New("result file identity mismatch")
			}
			if p.Terminal(rec.Report.State) && (rec.Report.Result == nil || rec.Report.State != header.State || journalManifest(rec) != header.Manifest) {
				return errors.New("conflicting unconfirmed result; preserving both")
			}
			if pending {
				final := a.spoolPath(id)
				if existing, other, _, e := readSpool(final, true); e == nil {
					existing.Close()
					if !equalSpool(other, header) {
						return errors.New("conflicting complete result files")
					}
					if e := os.Remove(path); e != nil {
						return e
					}
					if e := p.SyncDirectory(a.spoolDir()); e != nil {
						return e
					}
				} else if !os.IsNotExist(e) {
					return e
				} else if e := a.commitSpool(path, final); e != nil {
					return e
				}
			}
			rec.Task = compactTask(rec.Task)
			rec.Report = p.Report{TaskID: id, State: header.State, Result: &header.Result}
			rec.Spool = &header.Manifest
			records[id] = rec
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}
	// A missing referenced body is never converted to an empty result or rerun.
	for id, rec := range records {
		if rec.Spool != nil && !rec.Acknowledged {
			if _, err := os.Stat(a.spoolPath(id)); err != nil {
				return err
			}
		}
	}
	return nil
}
