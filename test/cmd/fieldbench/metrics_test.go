package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMetricsPreserveCountersAndRecordMalformedSamplesAsGaps(t *testing.T) {
	for _, tc := range []struct {
		status    int
		body      string
		available bool
	}{{200, `{"requests":100,"failures":4}`, true}, {503, `{"requests":0}`, false}, {200, "not-json", false}, {200, strings.Repeat(" ", 1<<20) + "{}", false}} {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer test-token" {
				t.Error("missing authorization")
			}
			w.WriteHeader(tc.status)
			w.Write([]byte(tc.body))
		}))
		sample := collectMetrics(context.Background(), s.Client(), s.URL, "test-token")
		s.Close()
		if sample.Available != tc.available || (!tc.available && len(sample.Data) != 0) {
			t.Fatal(sample)
		}
		b, _ := json.Marshal(sample)
		if strings.Contains(string(b), "test-token") {
			t.Fatal("token appeared in report")
		}
		if tc.available && !strings.Contains(string(sample.Data), `"requests":100`) {
			t.Fatal("counter changed")
		}
	}
}
