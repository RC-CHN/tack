package node

import (
	"context"
	"errors"
	"io"
	"os"
	"time"

	p "tack.local/management/protocol"
)

func (a *Node) upload(ctx context.Context, job journal) error {
	manifest := journalManifest(job)
	// V3 reads immutable disk output one chunk at a time. Legacy sessions slice
	// their in-memory output without making a second concatenated result copy.
	total := manifest.Total()
	retry := time.Second
	nextSend := time.Now()
	var body *os.File
	var bodyOffset int64
	var buffer []byte
	defer func() {
		if body != nil {
			body.Close()
		}
	}()
	for {
		input := p.ResultChunk{Version: p.TransferVersion, NodeID: a.config.NodeID, InstanceID: a.instance, TaskID: job.Task.ID, SHA256: manifest.SHA256, Probe: true}
		var ack p.ChunkAck
		var err error
		// Probe before touching the output. An unavailable controller must not
		// make every retry read/hash the whole spool or allocate a chunk buffer.
		ack, err = a.chunk(ctx, input, total)
		if err == nil {
			err = ctx.Err()
		}
		if err == nil && job.Spool != nil && body == nil {
			var header spoolHeader
			body, header, bodyOffset, err = readSpool(a.spoolPath(job.Task.ID), true)
			if err == nil && (header.TaskID != job.Task.ID || header.NodeID != job.Task.NodeID || header.Sequence != job.Task.Sequence || header.State != job.Report.State || header.Manifest != manifest) {
				body.Close()
				body = nil
				err = errors.New("result file changed; preserving unconfirmed result")
			}
			if err == nil && !ack.Complete {
				buffer = make([]byte, min(total, p.MaxChunk))
			}
		}
		for err == nil && !ack.Complete {
			if err = delay(ctx, time.Until(nextSend)); err != nil {
				return err
			}
			end := ack.Offset + p.MaxChunk
			if end > total {
				end = total
			}
			if end == ack.Offset {
				err = errors.New("controller did not commit complete output")
				break
			}
			input.Probe, input.Offset = false, ack.Offset
			if body != nil {
				input.Data = buffer[:end-ack.Offset]
				if n, e := body.ReadAt(input.Data, bodyOffset+int64(ack.Offset)); e != nil {
					err = e
					break
				} else if n != len(input.Data) {
					err = io.ErrUnexpectedEOF
					break
				}
			} else {
				input.Data = outputChunk(job.Report.Result, ack.Offset)
			}
			if rate := a.config.UploadBytesPerSecond; rate > 0 {
				nextSend = time.Now().Add(time.Duration(len(input.Data)) * time.Second / time.Duration(rate))
			}
			ack, err = a.chunk(ctx, input, total)
			if err == nil && ack.Offset < end {
				err = errors.New("controller did not acknowledge chunk")
			}
			if err == nil {
				retry = time.Second
			}
		}
		if err == nil {
			return nil
		}
		if body != nil {
			// Reopen and verify on retry, including after an operator repairs a
			// damaged file by atomic replacement. Do not pin an obsolete inode.
			body.Close()
			body, buffer = nil, nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if a.Logf != nil {
			a.Logf("result upload: %v", err)
		}
		if err := delay(ctx, retryDelay(retry)); err != nil {
			return err
		}
		retry = nextRetry(retry)
	}
}
func (a *Node) chunk(ctx context.Context, input p.ResultChunk, total int) (p.ChunkAck, error) {
	var ack p.ChunkAck
	err := a.request(ctx, a.uploadClient, p.NodeResultPath, input, &ack)
	if err == nil && (ack.Version != p.TransferVersion || ack.TaskID != input.TaskID || ack.Offset < 0 || ack.Offset > total || (ack.Complete && ack.Offset != total)) {
		err = errors.New("invalid upload acknowledgement")
	}
	return ack, err
}

// Only a chunk crossing the stdout/stderr boundary needs a temporary copy.
func outputChunk(result *p.Result, offset int) []byte {
	total := len(result.Stdout) + len(result.Stderr)
	end := offset + p.MaxChunk
	if end > total {
		end = total
	}
	if offset >= len(result.Stdout) {
		return result.Stderr[offset-len(result.Stdout) : end-len(result.Stdout)]
	}
	if end <= len(result.Stdout) {
		return result.Stdout[offset:end]
	}
	chunk := make([]byte, end-offset)
	n := copy(chunk, result.Stdout[offset:])
	copy(chunk[n:], result.Stderr[:end-len(result.Stdout)])
	return chunk
}
