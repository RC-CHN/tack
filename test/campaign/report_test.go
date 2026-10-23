package campaign

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

func TestHTTPCompletionRequiresStatusLengthAndDigest(t *testing.T) {
	want := []byte("verified response")
	sum := sha256.Sum256(want)
	for _, tc := range []struct {
		name    string
		status  int
		body    []byte
		failure string
	}{
		{"complete", 200, want, ""}, {"status", 503, want, "status"}, {"partial", 200, want[:3], "length"}, {"oversized", 200, append(bytes.Clone(want), '!'), "length"}, {"corrupt", 200, bytes.Repeat([]byte{'x'}, len(want)), "digest"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status); w.Write(tc.body) }))
			defer s.Close()
			v := HTTP(context.Background(), s.Client(), Target{"object", s.URL, int64(len(want)), hex.EncodeToString(sum[:])})
			if v.Success != (tc.failure == "") || v.Failure != tc.failure || v.Exact != v.Success {
				t.Fatal(v)
			}
		})
	}
}

func TestHTTPTimeoutAndErrorReportsDoNotLeakTarget(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	v := HTTP(ctx, s.Client(), Target{"safe-label", s.URL + "/secret?token=hidden", -1, ""})
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if v.Success || v.Failure != "timeout" || bytes.Contains(data, []byte("secret")) || bytes.Contains(data, []byte("hidden")) {
		t.Fatal(string(data))
	}
}

func TestCampaignCountsFailuresAndRequiresRecoveryTail(t *testing.T) {
	var a Aggregate
	for i := 0; i < 10; i++ {
		a.Add(Attempt{Success: true, Exact: true, Bytes: 100, DurationMS: float64(i)})
	}
	a.Add(Attempt{SetupFailure: true, Failure: "dial", DurationMS: 100})
	a.Add(Attempt{Failure: "length", Bytes: 50, DurationMS: 200})
	s := a.Snapshot()
	if s.Attempts != 12 || s.Completed != 10 || s.SetupFailures != 1 || s.TransportFailures != 1 || s.VerifiedBytes != 1000 || s.Bytes != 1050 || s.TrailingSuccesses != 0 {
		t.Fatal(s)
	}
	if Gate(s, .9, 0, 0) || Gate(s, .8, 0, 1) || Gate(s, .8, 10, 0) {
		t.Fatal("gate hid failure/tail/latency")
	}
	a.Add(Attempt{Success: true, DurationMS: 1})
	if !Gate(a.Snapshot(), .8, 0, 1) {
		t.Fatal("recovery did not satisfy gate")
	}
	data, _ := json.Marshal(s)
	var decoded Summary
	if err := json.Unmarshal(data, &decoded); err != nil || !reflect.DeepEqual(s, decoded) {
		t.Fatal("report round trip", err)
	}
}

func TestCampaignAggregationIsBoundedAndAllFailuresAreValid(t *testing.T) {
	var a Aggregate
	if Gate(a.Snapshot(), 0, 0, 0) {
		t.Fatal("empty campaign passed")
	}
	for i := 0; i < 20000; i++ {
		a.Add(Attempt{SetupFailure: true, DurationMS: float64(i)})
	}
	s := a.Snapshot()
	if s.Attempts != 20000 || s.Completed != 0 || s.SetupFailures != 20000 || s.Samples != SampleCapacity || len(a.first) != SampleCapacity || s.P50MS < 15000 {
		t.Fatal(s)
	}
	if Gate(s, 0, 0, 0) {
		t.Fatal("all failed campaign passed")
	}
}

func TestQuantileAndMatchedOrder(t *testing.T) {
	values := []float64{4, 1, 3, 2}
	copy := append([]float64(nil), values...)
	if Quantile(nil, .5) != 0 || Quantile(values, .5) != 2.5 || !reflect.DeepEqual(values, copy) {
		t.Fatal("quantile mutation/interpolation")
	}
	if !reflect.DeepEqual(Order(0, 2), []int{0, 1}) || !reflect.DeepEqual(Order(1, 2), []int{1, 0}) {
		t.Fatal("unmatched endpoint order")
	}
}

func BenchmarkCampaignAccounting(b *testing.B) {
	var a Aggregate
	v := Attempt{Success: true, Exact: true, Bytes: 1200, DurationMS: 1}
	for i := 0; i < SampleCapacity; i++ {
		a.Add(v)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		a.Add(v)
	}
}

func TestComparisonRequiresBothEndpointsAndCountsPartialTransfers(t *testing.T) {
	var a Aggregate
	for i := 0; i < 10; i++ {
		a.Add(Attempt{Success: true, Exact: true, Bytes: 100})
	}
	ref := a.Snapshot()
	if !Compare(ref, ref, time.Second, time.Second, 1, 1) {
		t.Fatal("parity failed")
	}
	if Compare(ref, Summary{}, time.Second, time.Second, 0, 0) || Compare(Summary{}, ref, time.Second, time.Second, 0, 0) {
		t.Fatal("missing endpoint passed")
	}
	if Compare(ref, ref, time.Second, 2*time.Second, 1, 1) {
		t.Fatal("goodput shortfall passed")
	}
	a.Add(Attempt{Bytes: 99, Failure: "length"})
	candidate := a.Snapshot()
	if Compare(ref, candidate, time.Second, time.Second, 1, 0) {
		t.Fatal("partial response hid completion shortfall")
	}
	candidate = ref
	candidate.VerifiedBytes = 0
	if Compare(ref, candidate, time.Second, time.Second, 0, .1) {
		t.Fatal("unverified bytes counted as goodput")
	}
}
