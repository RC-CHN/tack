package node

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	p "tack.local/management/protocol"
)

// The HTTP transport is deterministic; spool verification, request encoding,
// response decoding and cancellation are the production upload path. Setup and
// durable spool creation are excluded. This is not a network throughput test.
func BenchmarkSpoolUpload(b *testing.B) {
	for _, mode := range []string{"unavailable", "already_complete", "transfer"} {
		b.Run(mode, func(b *testing.B) {
			a, err := New(p.NodeConfig{Server: "https://controller.invalid", NodeID: "bench", Token: strings.Repeat("x", 40), StateDir: b.TempDir(), WorkDir: b.TempDir()})
			if err != nil {
				b.Fatal(err)
			}
			defer a.connections.close()
			a.Logf = nil
			rec, err := a.spoolRecord(journal{Task: p.Task{ID: "result", NodeID: "bench", Sequence: 1}, Report: p.Report{TaskID: "result", State: "succeeded", Result: &p.Result{Stdout: bytes.Repeat([]byte{'o'}, p.MaxOutput), Stderr: bytes.Repeat([]byte{'e'}, p.MaxOutput)}}})
			if err != nil {
				b.Fatal(err)
			}
			var cancel context.CancelFunc
			var offset, requests int
			a.uploadClient = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
				defer r.Body.Close()
				var in p.ResultChunk
				if err := p.Decode(r.Body, &in); err != nil {
					return nil, err
				}
				requests++
				if in.TaskID != rec.Task.ID || in.SHA256 != rec.Spool.SHA256 {
					return nil, errors.New("wrong identity")
				}
				if mode == "unavailable" {
					if !in.Probe || len(in.Data) != 0 {
						b.Fatal("outage sent output")
					}
					cancel() // End after one failed probe, without measuring backoff sleep.
					out := response(struct{}{})
					out.StatusCode = 503
					return out, nil
				}
				if mode == "already_complete" {
					offset = rec.Spool.Total()
				}
				if !in.Probe {
					if in.Offset != offset || len(in.Data) == 0 || len(in.Data) > p.MaxChunk {
						return nil, errors.New("wrong chunk offset/size")
					}
					for i, v := range in.Data {
						want := byte('o')
						if offset+i >= p.MaxOutput {
							want = 'e'
						}
						if v != want {
							return nil, errors.New("corrupt output")
						}
					}
					offset += len(in.Data)
				}
				return response(p.ChunkAck{Version: p.TransferVersion, TaskID: in.TaskID, Offset: offset, Complete: offset == rec.Spool.Total()}), nil
			})}
			readBefore, hasIO := benchmarkReadChars()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				var ctx context.Context
				ctx, cancel = context.WithCancel(context.Background())
				offset, requests = 0, 0
				err := a.upload(ctx, rec)
				cancel()
				if mode == "unavailable" {
					if !errors.Is(err, context.Canceled) || requests != 1 {
						b.Fatalf("probe: %v requests=%d", err, requests)
					}
				} else if err != nil || offset != rec.Spool.Total() {
					b.Fatal("upload incomplete", err, offset)
				}
			}
			b.StopTimer()
			if after, ok := benchmarkReadChars(); hasIO && ok {
				b.ReportMetric(float64(after-readBefore)/float64(b.N), "read-B/op")
			}
		})
	}
}

// Linux rchar includes cached file reads and the small measurement reads, not
// physical disk traffic. Omit this metric on systems without /proc.
func benchmarkReadChars() (uint64, bool) {
	data, err := os.ReadFile("/proc/self/io")
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if value, ok := strings.CutPrefix(line, "rchar: "); ok {
			n, err := strconv.ParseUint(value, 10, 64)
			return n, err == nil
		}
	}
	return 0, false
}

func BenchmarkReadyDelay(b *testing.B) {
	for _, d := range []time.Duration{0, -time.Second} {
		b.Run(fmt.Sprint(d), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if err := delay(context.Background(), d); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
