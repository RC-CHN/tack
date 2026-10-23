package node

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	p "tack.local/management/protocol"
)

func TestUploadAdaptsOnlyAfterConfirmedNoProgress(t *testing.T) {
	for _, lostACK := range []bool{false, true} {
		t.Run(fmt.Sprint("lost_ack=", lostACK), func(t *testing.T) {
			a, err := New(testConfig(t))
			if err != nil {
				t.Fatal(err)
			}
			defer a.connections.close()
			a.Logf = nil
			result := &p.Result{Stdout: bytes.Repeat([]byte{'o'}, p.MaxChunk+7), Stderr: bytes.Repeat([]byte{'e'}, p.MaxChunk+19)}
			want := append(append([]byte{}, result.Stdout...), result.Stderr...)
			var received []byte
			var sizes []int
			failed := false
			probes := 0
			a.uploadClient = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
				defer r.Body.Close()
				var in p.ResultChunk
				if err := p.Decode(r.Body, &in); err != nil {
					return nil, err
				}
				if in.Probe {
					probes++
				} else {
					sizes = append(sizes, len(in.Data))
					if in.Offset != len(received) || !bytes.Equal(in.Data, want[in.Offset:in.Offset+len(in.Data)]) {
						t.Fatal("incorrect adaptive offset/body")
					}
					if failed || lostACK {
						received = append(received, in.Data...)
					}
					if !failed {
						failed = true
						out := response(struct{}{})
						out.StatusCode = 503
						return out, nil
					}
				}
				return response(p.ChunkAck{Version: p.TransferVersion, TaskID: in.TaskID, Offset: len(received), Complete: len(received) == len(want)}), nil
			})}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := a.upload(ctx, journal{Task: p.Task{ID: "adaptive"}, Report: p.Report{Result: result}}); err != nil {
				t.Fatal(err)
			}
			next := p.MaxChunk / 4
			if lostACK {
				next = p.MaxChunk
			}
			if probes != 2 || len(sizes) < 2 || sizes[0] != p.MaxChunk || sizes[1] != next || !bytes.Equal(received, want) {
				t.Fatal("incorrect progress adaptation", probes, sizes)
			}
			for _, n := range sizes[1:] {
				if n > next {
					t.Fatal("chunk grew again within unstable result", sizes)
				}
			}
		})
	}
}

func TestAdaptiveSmallDiskResultKeepsMinimumChunkAndIntegrity(t *testing.T) {
	a, rec := spoolFixture(t)
	defer a.connections.close()
	a.Logf = nil
	rec.Report.Result.Stdout = bytes.Repeat([]byte{'s'}, 7001)
	rec.Report.Result.Stderr = nil
	saved, err := a.spoolRecord(rec)
	if err != nil {
		t.Fatal(err)
	}
	var sizes []int
	offset := 0
	a.uploadClient = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		defer r.Body.Close()
		var in p.ResultChunk
		if err := p.Decode(r.Body, &in); err != nil {
			return nil, err
		}
		if !in.Probe {
			sizes = append(sizes, len(in.Data))
			if len(sizes) == 1 {
				out := response(struct{}{})
				out.StatusCode = 503
				return out, nil
			}
			if in.Offset != offset || !bytes.Equal(in.Data, rec.Report.Result.Stdout[offset:offset+len(in.Data)]) {
				t.Fatal("disk chunk mismatch")
			}
			offset += len(in.Data)
		}
		return response(p.ChunkAck{Version: p.TransferVersion, TaskID: in.TaskID, Offset: offset, Complete: offset == 7001}), nil
	})}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.upload(ctx, saved); err != nil {
		t.Fatal(err)
	}
	if len(sizes) != 3 || sizes[0] != 7001 || sizes[1] != 4096 || sizes[2] != 2905 {
		t.Fatal("invalid minimum adaptive chunk", sizes)
	}
}

func TestProbeConfirmedProgressPreventsEscalatingBackoff(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.connections.close()
	a.Logf = nil
	const total = 5 * p.MaxChunk
	result := &p.Result{Stdout: bytes.Repeat([]byte{'p'}, total)}
	offset, probes := 0, 0
	a.uploadClient = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		defer r.Body.Close()
		var in p.ResultChunk
		if err := p.Decode(r.Body, &in); err != nil {
			return nil, err
		}
		if in.Probe {
			probes++
			return response(p.ChunkAck{Version: p.TransferVersion, TaskID: in.TaskID, Offset: offset, Complete: offset == total}), nil
		}
		if in.Offset != offset || len(in.Data) != p.MaxChunk || !bytes.Equal(in.Data, result.Stdout[offset:offset+len(in.Data)]) {
			t.Fatal("lost ACK caused duplicate or resized data")
		}
		offset += len(in.Data) // Commit the chunk, but lose every data ACK.
		out := response(struct{}{})
		out.StatusCode = 503
		return out, nil
	})}
	// Five initial retry windows total at most 7.5s. Escalating despite progress
	// requires at least 15.5s, independent of the jitter's random draws.
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	if err := a.upload(ctx, journal{Task: p.Task{ID: "progress"}, Report: p.Report{Result: result}}); err != nil {
		t.Fatal("progress incorrectly escalated backoff", err)
	}
	if offset != total || probes != 6 {
		t.Fatal("missing confirmed progress", offset, probes)
	}
}
