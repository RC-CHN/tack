package node

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	p "tack.local/management/protocol"
)

func TestBacklogAdmissionBoundaryAndAcknowledgement(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.connections.close()
	a.config.MaxPendingResults = 4
	a.config.MaxPendingResultBytes = 6 * p.MaxOutput
	for _, stored := range []int{0, 4*p.MaxOutput - 1, 4 * p.MaxOutput, 4*p.MaxOutput + 1} {
		records := map[string]journal{}
		for left := stored; left > 0; {
			n := min(left, 2*p.MaxOutput)
			left -= n
			records[fmt.Sprint(len(records))] = journal{Report: p.Report{State: "succeeded", Result: &p.Result{}}, Spool: &p.Manifest{StdoutBytes: min(n, p.MaxOutput), StderrBytes: max(0, n-p.MaxOutput)}}
		}
		got := a.nodeMetrics(records, "")
		if got.PendingResultBytes != stored || got.AcceptingTasks != (stored <= 4*p.MaxOutput) {
			t.Fatalf("stored=%d metrics=%+v", stored, got)
		}
		if a.nodeMetrics(records, "running").AcceptingTasks {
			t.Fatal("admitted while another task runs")
		}
		for id, rec := range records {
			records[id] = acknowledge(rec)
		}
		if got := a.nodeMetrics(records, ""); !got.AcceptingTasks || got.PendingResultBytes != 0 || got.PendingResults != 0 {
			t.Fatal("acknowledgement did not release budget", got)
		}
	}
}

func TestConnectionOwnerClosesConcurrentLateDials(t *testing.T) {
	owner := newConnectionSet()
	const count = 12
	entered := make(chan struct{}, count)
	release := make(chan struct{})
	peers := make(chan net.Conn, count)
	transport := &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		entered <- struct{}{}
		<-release
		one, two := net.Pipe()
		peers <- two
		return one, nil // Deliberately return a late success after cancellation.
	}}
	owner.attach(&http.Client{Transport: transport})
	var workers sync.WaitGroup
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer func() { unblock(); owner.close(); workers.Wait() }()
	for i := 0; i < count; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			conn, err := transport.DialContext(context.Background(), "tcp", "unused")
			if err == nil {
				conn.Close()
				t.Error("late connection escaped closed owner")
			}
		}()
	}
	for i := 0; i < count; i++ {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("dial did not enter")
		}
	}
	closed := make(chan struct{})
	go func() { owner.close(); close(closed) }()
	select {
	case <-owner.context.Done():
	case <-time.After(time.Second):
		t.Fatal("owner did not cancel dials")
	}
	unblock()
	workers.Wait()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("close failed to join dials")
	}
	for i := 0; i < count; i++ {
		c := <-peers
		c.SetReadDeadline(time.Now().Add(time.Second))
		if _, err := c.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
			t.Error("late socket remained open", err)
		}
		c.Close()
	}
	if len(owner.connections) != 0 {
		t.Fatal("retained connections")
	}
}

func TestClosedConnectionOwnerNeverDials(t *testing.T) {
	owner := newConnectionSet()
	var calls atomic.Int32
	transport := &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
		calls.Add(1)
		return nil, errors.New("unexpected")
	}}
	owner.attach(&http.Client{Transport: transport})
	owner.close()
	for i := 0; i < 50; i++ {
		if _, err := transport.DialContext(context.Background(), "tcp", "unused"); !errors.Is(err, net.ErrClosed) {
			t.Fatal(err)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("closed owner started a dial")
	}
}

func TestRetryWindowsAndCancellationBudget(t *testing.T) {
	base := time.Second
	values := map[time.Duration]bool{}
	for round := 0; round < 16; round++ {
		for i := 0; i < 100; i++ {
			v := retryDelay(base)
			if v < base/2 || v > base+base/2 {
				t.Fatalf("retry outside window: %v base %v", v, base)
			}
			values[v] = true
		}
		base = nextRetry(base)
		if base > 30*time.Second {
			t.Fatal("unbounded backoff")
		}
	}
	if len(values) < 100 {
		t.Fatal("retry jitter did not spread requests")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if err := delay(ctx, time.Hour); !errors.Is(err, context.Canceled) || time.Since(start) > time.Second {
		t.Fatal("cancel waited for retry window", err)
	}
}

func TestRequestFailureClassificationRedactsAddressAndToken(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.connections.close()
	for _, tc := range []struct {
		err  error
		want string
	}{
		{context.Canceled, "canceled"}, {&net.DNSError{Err: "secret", Name: "private-host", IsNotFound: true}, "dns"},
		{&net.DNSError{Err: "secret", Name: "private-host", IsTimeout: true}, "timeout"}, {errors.New("private-host secret"), "transport"},
	} {
		client := &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) { return nil, tc.err })}
		err := a.request(context.Background(), client, "/health", nil, &struct{}{})
		if err == nil || !strings.HasSuffix(err.Error(), tc.want) || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "private-host") {
			t.Fatal(err)
		}
	}
}

func BenchmarkBacklogMetrics(b *testing.B) {
	for _, size := range []int{0, 128, 8192} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			a, err := New(p.NodeConfig{Server: "https://controller.invalid", NodeID: "bench", Token: strings.Repeat("x", 40), StateDir: b.TempDir(), WorkDir: b.TempDir()})
			if err != nil {
				b.Fatal(err)
			}
			defer a.connections.close()
			records := make(map[string]journal, size)
			for i := 0; i < size; i++ {
				records[fmt.Sprint(i)] = journal{Acknowledged: true, Report: p.Report{State: "succeeded"}}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if got := a.nodeMetrics(records, ""); got.JournalRecords != size {
					b.Fatal(got)
				}
			}
		})
	}
}

func TestNegotiationRejectsOldVersionAndBoundsNewVersion(t *testing.T) {
	for _, version := range []int{0, 1, 2, 3, 99} {
		a, err := New(testConfig(t))
		if err != nil {
			t.Fatal(err)
		}
		a.client = &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
			return response(map[string]int{"protocol_version": version}), nil
		})}
		err = a.negotiate(context.Background())
		a.connections.close()
		if version < 1 {
			if err == nil {
				t.Fatal("obsolete protocol accepted")
			}
			continue
		}
		if err != nil || a.version != min(version, p.LatestVersion) {
			t.Fatal("invalid negotiated version", version, a.version, err)
		}
	}
}
