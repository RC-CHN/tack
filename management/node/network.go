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
		return &requestFailure{kind: kind}
	}
	defer resp.Body.Close()
	status = resp.StatusCode
	if status != 200 {
		kind = "http"
		return fmt.Errorf("management request returned HTTP %d", status)
	}
	limited := &io.LimitedReader{R: resp.Body, N: p.MaxBody}
	if err := p.Decode(limited, output); err != nil {
		var networkError net.Error
		truncated := limited.N > 0 && resp.ContentLength > int64(p.MaxBody)-limited.N && (errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF))
		if truncated || errors.As(err, &networkError) {
			kind = errorClass(err)
			return &requestFailure{kind: kind}
		}
		kind = "decode"
		return errors.New("invalid management response")
	}
	return nil
}

// Preserve a redacted transport classification for retry policy, without
// retaining the URL, credential, or provider's original error text.
type requestFailure struct{ kind string }

func (e *requestFailure) Error() string { return "management request failed: " + e.kind }
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
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	if errors.As(err, &cert) || errors.As(err, &authority) || errors.As(err, &hostname) || errors.As(err, &invalid) {
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
			if health.Version >= p.RetentionVersion {
				a.version = p.RetentionVersion
			}
			if health.Version >= p.OperationsVersion {
				a.version = p.OperationsVersion
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
	var status *p.NodeStatus
	if a.version >= p.OperationsVersion {
		status = a.statusSnapshot(metrics)
	}
	return a.syncSnapshot(ctx, report, metrics, high, receipts, status)
}

func (a *Node) syncSnapshot(ctx context.Context, report *p.Report, metrics p.NodeMetrics, high uint64, receipts []string, status *p.NodeStatus) (p.SyncResponse, error) {
	host, _ := os.Hostname()
	input := p.SyncRequest{Version: a.version, InstanceID: a.instance, Node: p.Node{ID: a.config.NodeID, Hostname: host, OS: runtime.GOOS, Arch: runtime.GOARCH, Modes: a.runner.Modes(), StatePersistence: a.config.StatePersistence}, Report: report}
	if a.version == p.Version && report != nil && report.Result != nil {
		copy := *report
		result := *report.Result
		result.ErrorCode, result.Phase, result.Cleanup = "", "", nil
		copy.Result = &result
		input.Report = &copy
	}
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
			if report.Upload != nil {
				manifest = *report.Upload
			}
			metadata.Stdout, metadata.Stderr = nil, nil
			boundResultMetadata(&metadata)
			if a.version < p.OperationsVersion {
				metadata.ErrorCode, metadata.Phase, metadata.Cleanup = "", "", nil
			}
			value.Result, value.Upload = &metadata, &manifest
			input.Report = &value
		}
	}
	if a.version >= p.RetentionVersion {
		path = p.NodeRetentionPath
		input.ReplayHigh, input.Receipts = high, receipts
	}
	if a.version >= p.OperationsVersion {
		path = p.NodeOperationsPath
		input.Node.Status = status
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
	if err := ctx.Err(); err != nil {
		return err
	}
	if d <= 0 {
		return nil
	}
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
