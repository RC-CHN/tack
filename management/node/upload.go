package node

import (
	"context"
	"errors"
	"time"

	p "tack.local/management/protocol"
)

func (a *Node) upload(ctx context.Context, job journal) error {
	manifest := p.OutputManifest(job.Report.Result)
	// The runner output is immutable. Slice it directly instead of allocating
	// another full stdout+stderr buffer for every pending upload.
	total := manifest.Total()
	retry := time.Second
	nextSend := time.Now()
	for {
		input := p.ResultChunk{Version: p.TransferVersion, NodeID: a.config.NodeID, InstanceID: a.instance, TaskID: job.Task.ID, SHA256: manifest.SHA256, Probe: true}
		ack, err := a.chunk(ctx, input, total)
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
			input.Probe, input.Offset, input.Data = false, ack.Offset, outputChunk(job.Report.Result, ack.Offset)
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
