package node

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"

	p "tack.local/management/protocol"
)

func (a *Node) request(ctx context.Context, client *http.Client, path string, input, output any) (err error) {
	start := time.Now()
	status, kind := 0, ""
	defer func() { a.meter.Request(path == p.NodeResultPath, start, status, kind) }()
	method := "GET"
	var body []byte
	if input != nil {
		method = "POST"
		body, err = json.Marshal(input)
		if err != nil {
			kind = "encode"
			return errors.New("cannot encode management request")
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, a.origin+path, bytes.NewReader(body))
	if err != nil {
		kind = "request"
		return errors.New("invalid management request")
	}
	req.Header.Set("Authorization", "Bearer "+a.config.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		kind = errorClass(err)
		return fmt.Errorf("management request failed: %s", kind)
	}
	defer resp.Body.Close()
	status = resp.StatusCode
	if status != 200 {
		kind = "http"
		return fmt.Errorf("management request returned HTTP %d", status)
	}
	if err := p.Decode(io.LimitReader(resp.Body, p.MaxBody), output); err != nil {
		kind = "decode"
		return errors.New("invalid management response")
	}
	return nil
}
func errorClass(err error) string {
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		return "timeout"
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return "dns"
	}
	var cert *tls.CertificateVerificationError
	var authority x509.UnknownAuthorityError
	if errors.As(err, &cert) || errors.As(err, &authority) {
		return "tls"
	}
	return "transport"
}
func (a *Node) negotiate(ctx context.Context) error {
	retry := time.Second
	for {
		var health struct {
			Version int `json:"protocol_version"`
		}
		err := a.request(ctx, a.client, "/health", nil, &health)
		if err == nil {
			if health.Version < p.Version {
				return errors.New("unsupported controller protocol version")
			}
			a.version = p.Version
			if health.Version >= p.TransferVersion {
				a.version = p.TransferVersion
			}
			if health.Version >= p.LatestVersion {
				a.version = p.LatestVersion
			}
			return nil
		}
		if a.Logf != nil {
			a.Logf("controller discovery: %v", err)
		}
		if err := delay(ctx, retryDelay(retry)); err != nil {
			return err
		}
		retry = nextRetry(retry)
	}
}
func (a *Node) sync(ctx context.Context, report *p.Report, metrics p.NodeMetrics) (p.SyncResponse, error) {
	return a.syncRetention(ctx, report, metrics, 0, nil)
}

func (a *Node) syncRetention(ctx context.Context, report *p.Report, metrics p.NodeMetrics, high uint64, receipts []string) (p.SyncResponse, error) {
	host, _ := os.Hostname()
	input := p.SyncRequest{Version: a.version, InstanceID: a.instance, Node: p.Node{ID: a.config.NodeID, Hostname: host, OS: runtime.GOOS, Arch: runtime.GOARCH, Modes: a.runner.Modes(), StatePersistence: a.config.StatePersistence}, Report: report}
	path := p.NodeSyncPath
	client := *a.client // Same control transport, with a timeout covering this long poll.
	if a.version >= p.TransferVersion {
		path = p.NodeControlPath
		input.Node.Metrics = &metrics
		input.AcceptTask = metrics.AcceptingTasks
		input.WaitSeconds = heartbeatWait(a.config.HeartbeatSeconds)
		input.LeaseSeconds = a.config.LeaseSeconds
		client.Timeout = time.Duration(input.WaitSeconds+15) * time.Second
		if report != nil && report.Result != nil {
			metadata, value := *report.Result, *report
			manifest := p.OutputManifest(report.Result)
			metadata.Stdout, metadata.Stderr = nil, nil
			metadata.Error, metadata.MetadataTruncated = boundedMetadata(metadata.Error, 8192, metadata.MetadataTruncated)
			metadata.Executable, metadata.MetadataTruncated = boundedMetadata(metadata.Executable, 4096, metadata.MetadataTruncated)
			value.Result, value.Upload = &metadata, &manifest
			input.Report = &value
		}
	}
	if a.version >= p.LatestVersion {
		path = p.NodeRetentionPath
		input.ReplayHigh, input.Receipts = high, receipts
	}
	var output p.SyncResponse
	if err := a.request(ctx, &client, path, input, &output); err != nil {
		return output, err
	}
	if output.Version != a.version {
		return output, errors.New("unsupported controller protocol version")
	}
	return output, nil
}

// Jitter spreads fleet load. It does not change command delivery latency.
func heartbeatWait(seconds int) int { return seconds/2 + rand.Intn(seconds+1) }
func retryDelay(base time.Duration) time.Duration {
	return base/2 + time.Duration(rand.Int63n(int64(base)+1))
}
func nextRetry(base time.Duration) time.Duration {
	if base >= 30*time.Second {
		return 30 * time.Second
	}
	base *= 2
	if base > 30*time.Second {
		base = 30 * time.Second
	}
	return base
}
func delay(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func boundedMetadata(value string, limit int, truncated bool) (string, bool) {
	value = strings.ToValidUTF8(value, "\uFFFD")
	if len(value) <= limit {
		return value, truncated
	}
	const suffix = " [truncated]"
	prefix := value[:limit-len(suffix)]
	for !utf8.ValidString(prefix) {
		prefix = prefix[:len(prefix)-1]
	}
	return prefix + suffix, true
}
