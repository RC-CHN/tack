package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"
)

type metricSample struct {
	Kind      string          `json:"kind"`
	At        time.Time       `json:"at"`
	ElapsedMS int64           `json:"elapsed_ms"`
	Available bool            `json:"available"`
	Failure   string          `json:"failure,omitempty"`
	Status    int             `json:"status"`
	Data      json.RawMessage `json:"data,omitempty"`
}

// Collect Tack's authenticated JSON metrics. Invalid/failed samples remain
// explicit gaps, never zero-valued counter observations. Tokens are not logged.
func collectMetrics(ctx context.Context, client *http.Client, url, token string) metricSample {
	out := metricSample{Kind: "metrics", At: time.Now().UTC()}
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		out.Failure = "request"
		return out
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	r, err := client.Do(req)
	if err != nil {
		out.Failure = "transport"
		return out
	}
	defer r.Body.Close()
	out.Status = r.StatusCode
	if r.StatusCode != 200 {
		out.Failure = "status"
		return out
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, (1<<20)+1))
	if err != nil || len(data) > 1<<20 || !json.Valid(data) {
		out.Failure = "body"
		return out
	}
	out.Available = true
	out.Data = data
	return out
}
