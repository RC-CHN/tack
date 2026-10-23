package node

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	p "tack.local/management/protocol"
)

func TestUnavailableUploadDoesNotReadSpool(t *testing.T) {
	a, rec := spoolFixture(t)
	defer a.connections.close()
	saved, err := a.spoolRecord(rec)
	if err != nil {
		t.Fatal(err)
	}
	// A bad local body distinguishes a probe from prematurely reading output.
	// Recovery can diagnose this after connectivity returns; outage retries
	// must not read the body or allocate output buffers.
	if err := os.Truncate(a.spoolPath(rec.Task.ID), 0); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	probes := 0
	a.uploadClient = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		defer r.Body.Close()
		var in p.ResultChunk
		if err := p.Decode(r.Body, &in); err != nil {
			return nil, err
		}
		if !in.Probe || len(in.Data) != 0 {
			t.Error("output sent during outage")
		}
		probes++
		out := response(struct{}{})
		out.StatusCode = 503
		return out, nil
	})}
	var failure string
	a.Logf = func(format string, args ...any) { failure = fmt.Sprintf(format, args...); cancel() }
	if err := a.upload(ctx, saved); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if probes != 1 || !strings.Contains(failure, "HTTP 503") {
		t.Fatalf("probe=%d failure=%q", probes, failure)
	}
}

func TestSuccessfulProbeStillVerifiesSpoolBeforeSendOrCompletion(t *testing.T) {
	for _, complete := range []bool{false, true} {
		t.Run(fmt.Sprint(complete), func(t *testing.T) {
			a, rec := spoolFixture(t)
			defer a.connections.close()
			saved, err := a.spoolRecord(rec)
			if err != nil {
				t.Fatal(err)
			}
			path := a.spoolPath(rec.Task.ID)
			file, err := os.OpenFile(path, os.O_WRONLY, 0600)
			if err != nil {
				t.Fatal(err)
			}
			info, err := file.Stat()
			if err != nil {
				file.Close()
				t.Fatal(err)
			}
			_, err = file.WriteAt([]byte("!"), info.Size()-1)
			file.Close()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			probes := 0
			a.uploadClient = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
				defer r.Body.Close()
				var in p.ResultChunk
				if err := p.Decode(r.Body, &in); err != nil {
					return nil, err
				}
				if !in.Probe || len(in.Data) != 0 {
					t.Error("corrupt output escaped verification")
				}
				probes++
				offset := 0
				if complete {
					offset = saved.Spool.Total()
				}
				return response(p.ChunkAck{Version: p.TransferVersion, TaskID: rec.Task.ID, Complete: complete, Offset: offset}), nil
			})}
			var failure string
			a.Logf = func(format string, args ...any) { failure = fmt.Sprintf(format, args...); cancel() }
			if err := a.upload(ctx, saved); !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if probes != 1 || !strings.Contains(failure, "digest mismatch") {
				t.Fatalf("probe=%d failure=%q", probes, failure)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatal("unconfirmed result discarded", err)
			}
		})
	}
}

func TestReadyDelayHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, duration := range []time.Duration{-time.Second, 0, time.Hour} {
		for i := 0; i < 100; i++ {
			if err := delay(ctx, duration); !errors.Is(err, context.Canceled) {
				t.Fatal("ready timer hid cancellation", err)
			}
		}
	}
	start := time.Now()
	if err := delay(context.Background(), 10*time.Millisecond); err != nil || time.Since(start) < 10*time.Millisecond {
		t.Fatal("positive rate limit skipped", err)
	}
}
