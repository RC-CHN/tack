// Package campaign provides bounded measurement and failure accounting for Tack
// campaigns. It is test tooling, never a production dependency.
package campaign

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptrace"
	"slices"
	"sync/atomic"
	"time"
)

type Attempt struct {
	Round        int     `json:"round"`
	Endpoint     string  `json:"endpoint"`
	Target       string  `json:"target"`
	Kind         string  `json:"kind"`
	Success      bool    `json:"success"`
	Exact        bool    `json:"exact_content_verified"`
	SetupFailure bool    `json:"setup_failure"`
	Failure      string  `json:"failure,omitempty"`
	Status       int     `json:"status,omitempty"`
	Bytes        int64   `json:"bytes"`
	DurationMS   float64 `json:"duration_ms"`
	FirstByteMS  float64 `json:"first_byte_ms"`
}

type Target struct {
	Name string `json:"name"`
	URL  string `json:"url"`
	// Bytes = -1 is an availability probe, not an exact transfer benchmark.
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// HTTP performs one attempt with no retries. Callers provide the deadline and
// redirect policy. URLs and transport error strings never enter the report.
func HTTP(ctx context.Context, client *http.Client, target Target) (out Attempt) {
	out.Target, out.Kind = target.Name, "http"
	start := time.Now()
	var first atomic.Int64
	defer func() {
		out.DurationMS = float64(time.Since(start)) / float64(time.Millisecond)
		out.FirstByteMS = float64(first.Load()) / float64(time.Millisecond)
	}()
	trace := &httptrace.ClientTrace{GotFirstResponseByte: func() { first.CompareAndSwap(0, time.Since(start).Nanoseconds()) }}
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), "GET", target.URL, nil)
	if err != nil {
		out.SetupFailure = true
		out.Failure = "request"
		return
	}
	r, err := client.Do(req)
	if err != nil {
		out.Failure = "transport"
		if errors.Is(err, context.DeadlineExceeded) {
			out.Failure = "timeout"
		}
		if errors.Is(err, context.Canceled) {
			out.Failure = "canceled"
		}
		return
	}
	defer r.Body.Close()
	out.Status = r.StatusCode
	limit := target.Bytes
	if limit < 0 {
		limit = 1 << 20
	}
	if limit > 1<<30 {
		out.SetupFailure = true
		out.Failure = "body_limit"
		return
	}
	hash := sha256.New()
	out.Bytes, err = io.CopyBuffer(hash, io.LimitReader(r.Body, limit+1), make([]byte, 16<<10))
	if err != nil {
		out.Failure = "body"
		return
	}
	if r.StatusCode < 200 || r.StatusCode >= 300 || (target.Bytes >= 0 && r.StatusCode != 200) {
		out.Failure = "status"
		return
	}
	if out.Bytes > limit || (target.Bytes >= 0 && out.Bytes != target.Bytes) {
		out.Failure = "length"
		return
	}
	if target.SHA256 != "" && hex.EncodeToString(hash.Sum(nil)) != target.SHA256 {
		out.Failure = "digest"
		return
	}
	out.Success = true
	out.Exact = target.Bytes >= 0 && target.SHA256 != ""
	return
}

const SampleCapacity = 4096

type Summary struct {
	Attempts, Completed, SetupFailures, TransportFailures uint64
	Bytes, VerifiedBytes                                  uint64
	TrailingSuccesses                                     uint64
	DurationMS                                            float64
	Samples                                               int
	P50MS, P95MS, P99MS, FirstByteP95MS                   float64
	SuccessRate                                           float64
	// Latency quantiles cover the latest SampleCapacity attempts, including
	// failures. Counts and bytes cover the entire campaign.
	LatencyWindow string
}

type Aggregate struct {
	summary        Summary
	latency, first []float64
	index          int
}

func (a *Aggregate) Add(v Attempt) {
	s := &a.summary
	s.Attempts++
	s.DurationMS += v.DurationMS
	if v.Bytes > 0 {
		s.Bytes += uint64(v.Bytes)
	}
	if v.Success {
		s.Completed++
		s.TrailingSuccesses++
		if v.Exact {
			s.VerifiedBytes += uint64(v.Bytes)
		}
	} else {
		s.TrailingSuccesses = 0
		if v.SetupFailure {
			s.SetupFailures++
		} else {
			s.TransportFailures++
		}
	}
	if len(a.latency) < SampleCapacity {
		a.latency = append(a.latency, v.DurationMS)
		a.first = append(a.first, v.FirstByteMS)
	} else {
		a.latency[a.index] = v.DurationMS
		a.first[a.index] = v.FirstByteMS
	}
	a.index = (a.index + 1) % SampleCapacity
}

// Quantile interpolates without changing caller-owned samples.
func Quantile(values []float64, q float64) float64 {
	if len(values) == 0 {
		return 0
	}
	copy := slices.Clone(values)
	slices.Sort(copy)
	x := max(0, min(1, q)) * float64(len(copy)-1)
	i := int(math.Floor(x))
	j := int(math.Ceil(x))
	return copy[i] + (copy[j]-copy[i])*(x-float64(i))
}

func (a *Aggregate) Snapshot() Summary {
	s := a.summary
	s.Samples = len(a.latency)
	s.LatencyWindow = "latest-4096-attempts-including-failures"
	s.P50MS = Quantile(a.latency, .5)
	s.P95MS = Quantile(a.latency, .95)
	s.P99MS = Quantile(a.latency, .99)
	s.FirstByteP95MS = Quantile(a.first, .95)
	if s.Attempts > 0 {
		s.SuccessRate = float64(s.Completed) / float64(s.Attempts)
	}
	return s
}

func Gate(s Summary, minSuccess float64, maxP95MS float64, tail uint64) bool {
	return s.Attempts > 0 && s.Completed > 0 && s.SuccessRate >= minSuccess && s.TrailingSuccesses >= tail && (maxP95MS <= 0 || s.P95MS <= maxP95MS)
}

// Compare uses separately measured endpoint wall time, never the sum of
// concurrent request durations. Unverified bytes cannot satisfy a goodput gate.
func Compare(reference, candidate Summary, referenceWall, candidateWall time.Duration, minCompletionRatio, minGoodputRatio float64) bool {
	if reference.Attempts == 0 || candidate.Attempts == 0 || reference.Completed == 0 || candidate.Completed == 0 || referenceWall <= 0 || candidateWall <= 0 {
		return false
	}
	if candidate.SuccessRate < reference.SuccessRate*minCompletionRatio {
		return false
	}
	if minGoodputRatio > 0 {
		if reference.VerifiedBytes == 0 || candidate.VerifiedBytes == 0 {
			return false
		}
		if float64(candidate.VerifiedBytes)/candidateWall.Seconds() < float64(reference.VerifiedBytes)/referenceWall.Seconds()*minGoodputRatio {
			return false
		}
	}
	return true
}

// Order alternates which endpoint goes first in matched trials.
func Order(round, count int) []int {
	order := make([]int, count)
	for i := range order {
		order[i] = i
	}
	if round%2 != 0 {
		slices.Reverse(order)
	}
	return order
}
