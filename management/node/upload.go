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
	var lastProgress, lastResponse time.Time
	nextSend := time.Now()
	chunkLimit := p.MaxChunk
	failedOffset, failedBytes := -1, 0
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
		if err == nil {
			lastResponse = time.Now()
		}
		if err == nil && failedOffset >= 0 {
			// A failed request may have committed its data and only lost the ACK.
			// Reduce the checkpoint size only when a successful probe proves that
			// the failed data request made no durable progress. Keep the smaller
			// size for this result so intermittent links do not oscillate back
			// into repeatedly unsendable chunks. Clean links retain MaxChunk.
			if ack.Offset > failedOffset {
				// This probe is the acknowledgement lost with the connection.
				// Durable progress must reset backoff just like a direct ACK;
				// otherwise repeated lost ACKs make a working link wait longer
				// and longer even as the receiver accumulates valid output.
				retry = time.Second
				lastProgress = time.Now()
				lastResponse = lastProgress
			} else if failedBytes > 4*1024 {
				chunkLimit = max(4*1024, failedBytes/4)
			}
			failedOffset = -1
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
				buffer = make([]byte, min(total, chunkLimit))
			}
		}
		for err == nil && !ack.Complete {
			if err = delay(ctx, time.Until(nextSend)); err != nil {
				return err
			}
			end := ack.Offset + chunkLimit
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
				input.Data = outputChunkSize(job.Report.Result, ack.Offset, chunkLimit)
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
				lastProgress = time.Now()
				lastResponse = lastProgress
			} else {
				failedOffset, failedBytes = input.Offset, len(input.Data)
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
		if err := delay(ctx, uploadRetryDelay(retry, lastProgress, lastResponse, err)); err != nil {
			return err
		}
		retry = nextRetry(retry)
	}
}

// A few failed ACKs on an intermittent link must not turn recent durable
// progress into a 15..45s pause. For 20s after confirmed progress, cap the
// jitter's base at 2s. The underlying backoff continues growing, so a sustained
// outage returns to the normal quiet retry schedule when that window expires.
// A verified probe also demonstrates reachability when the next failure is a
// broken transport. This matters before the very first data ACK. HTTP errors,
// certificate errors and malformed replies cannot keep renewing that window.
func uploadRetryDelay(base time.Duration, lastProgress, lastResponse time.Time, err error) time.Duration {
	var failed *requestFailure
	if errors.As(err, &failed) && (failed.kind == "transport" || failed.kind == "timeout" || failed.kind == "dns") && lastResponse.After(lastProgress) {
		lastProgress = lastResponse
	}
	if !lastProgress.IsZero() && time.Since(lastProgress) < 20*time.Second {
		base = min(base, 2*time.Second)
	}
	return retryDelay(base)
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
	return outputChunkSize(result, offset, p.MaxChunk)
}

func outputChunkSize(result *p.Result, offset, limit int) []byte {
	total := len(result.Stdout) + len(result.Stderr)
	end := offset + limit
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
