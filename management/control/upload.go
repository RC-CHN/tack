package control

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	p "tack.local/management/protocol"
)

// A slow request body or chunk flush never holds the task queue mutex.
func (s *Server) uploadResult(w http.ResponseWriter, r *http.Request, id string) {
	// Only authenticated uploads receive an extended body/response deadline.
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(p.ResultTimeoutSeconds * time.Second))
	_ = rc.SetWriteDeadline(time.Now().Add((p.ResultTimeoutSeconds + 20) * time.Second))
	var input p.ResultChunk
	if err := p.Decode(http.MaxBytesReader(w, r.Body, 2*p.MaxChunk), &input); err != nil {
		failure(w, 400, "invalid result chunk")
		return
	}
	if input.Version != p.LatestVersion || input.NodeID != id || !p.ValidID(input.InstanceID) || !p.ValidID(input.TaskID) || input.Offset < 0 || len(input.Data) > p.MaxChunk || (input.Probe && len(input.Data) != 0) {
		failure(w, 400, "invalid chunk identity, offset or size")
		return
	}
	hash := sha256.Sum256([]byte(input.TaskID))
	lock := &s.uploads[int(hash[0])%len(s.uploads)]
	lock.Lock()
	defer lock.Unlock()
	task, exists, lookupErr := s.lookupTask(input.TaskID)
	if lookupErr != nil {
		failure(w, 500, "cannot read result task")
		return
	}
	s.mu.Lock()
	instance := s.nodes[id].instance
	s.mu.Unlock()
	if !exists || task.NodeID != id {
		failure(w, 403, "task does not belong to this node")
		return
	}
	if instance != input.InstanceID {
		failure(w, 409, "synchronize this node instance first")
		return
	}
	if task.Result == nil || task.Upload == nil || task.Upload.SHA256 != input.SHA256 || task.Upload.Validate(task.Command.OutputLimit) != nil || !p.Terminal(task.State) {
		failure(w, 409, "no matching committed result manifest")
		return
	}
	total := task.Upload.Total()
	if input.Offset > total || len(input.Data) > total-input.Offset {
		failure(w, 400, "chunk exceeds result length")
		return
	}
	if !task.ResultPending {
		if err := s.removeUpload(task); err != nil {
			failure(w, 500, "cannot clean completed upload")
			return
		}
		respond(w, 200, p.ChunkAck{Version: p.LatestVersion, TaskID: task.ID, Offset: total, Complete: true})
		return
	}
	dir := s.path + ".uploads"
	if err := os.MkdirAll(dir, 0700); err != nil {
		failure(w, 500, "cannot create upload storage")
		return
	}
	if err := p.SyncDirectory(filepath.Dir(dir)); err != nil {
		failure(w, 500, "cannot persist upload directory")
		return
	}
	path := filepath.Join(dir, task.ID+"."+task.Upload.SHA256+".part")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		failure(w, 500, "cannot open upload storage")
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.Size() > int64(total) {
		failure(w, 500, "invalid upload storage")
		return
	}
	offset := int(info.Size())
	if !input.Probe {
		if len(input.Data) == 0 || input.Offset > offset {
			failure(w, 409, "probe current upload offset")
			return
		}
		if input.Offset < offset {
			previous := make([]byte, len(input.Data))
			if input.Offset+len(input.Data) > offset {
				failure(w, 409, "chunk overlaps stored boundary")
				return
			}
			if _, err := f.ReadAt(previous, int64(input.Offset)); err != nil || !bytes.Equal(previous, input.Data) {
				failure(w, 409, "chunk conflicts with stored output")
				return
			}
		} else {
			n, err := f.WriteAt(input.Data, int64(offset))
			if err == nil && n != len(input.Data) {
				err = io.ErrShortWrite
			}
			if err == nil {
				err = f.Sync()
			}
			if err != nil {
				_ = f.Truncate(int64(offset))
				_ = f.Sync()
				failure(w, 500, "cannot persist chunk")
				return
			}
			offset += len(input.Data)
		}
	}
	// Probe acknowledgements also flush recovered data before advertising it.
	if err := f.Sync(); err != nil {
		failure(w, 500, "cannot persist upload offset")
		return
	}
	if err := p.SyncDirectory(dir); err != nil {
		failure(w, 500, "cannot persist upload entry")
		return
	}
	if offset == total {
		data := make([]byte, total)
		if _, err := f.ReadAt(data, 0); err != nil && !errors.Is(err, io.EOF) {
			failure(w, 500, "cannot read output")
			return
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != task.Upload.SHA256 {
			// Re-send from zero after corruption; the task itself is never re-executed.
			_ = f.Truncate(0)
			_ = f.Sync()
			failure(w, 409, "output digest mismatch; probe and retry")
			return
		}
		result := *task.Result
		result.Stdout, result.Stderr = data[:task.Upload.StdoutBytes], data[task.Upload.StdoutBytes:]
		task.Result, task.ResultPending = &result, false
		task.Timing = timingCopy(task)
		task.Timing.ResultReceivedAt = time.Now().UTC()
		task.Timing.ResultTransferMS = task.Timing.ResultReceivedAt.Sub(task.Timing.TerminalReportedAt).Milliseconds()
		s.tx.Lock()
		s.mu.Lock()
		err = s.commit(task)
		s.mu.Unlock()
		s.tx.Unlock()
		if err != nil {
			failure(w, 500, "cannot commit complete result")
			return
		}
		if err := f.Close(); err != nil {
			failure(w, 500, "cannot close completed upload")
			return
		}
		if err := s.removeUpload(task); err != nil {
			failure(w, 500, "cannot clean completed upload")
			return
		}
	}
	respond(w, 200, p.ChunkAck{Version: p.LatestVersion, TaskID: task.ID, Offset: offset, Complete: offset == total})
}

func (s *Server) removeUpload(task p.Task) error {
	dir := s.path + ".uploads"
	path := filepath.Join(dir, task.ID+"."+task.Upload.SHA256+".part")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := p.SyncDirectory(dir); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
