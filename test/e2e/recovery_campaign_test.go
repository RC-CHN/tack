package e2e_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	chclient "github.com/jpillora/chisel/client"
	chserver "github.com/jpillora/chisel/server"
	"tack.local/testfixtures/faultnet"
)

func campaignTunnel(t testing.TB, reverse bool, options faultnet.Options) (string, *faultnet.Proxy) {
	t.Helper()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(io.LimitReader(r.Body, (1<<20)+1))
		if err != nil || len(data) > 1<<20 {
			http.Error(w, "invalid benchmark body", 400)
			return
		}
		w.Write(data)
	}))
	t.Cleanup(origin.Close)
	ctx, cancel := context.WithCancel(context.Background())
	server, err := chserver.NewServer(&chserver.Config{Reverse: reverse})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	server.SetOutput(io.Discard)
	port := availablePort()
	if err = server.StartContext(ctx, "127.0.0.1", port); err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); server.Wait() })
	link, err := faultnet.NewConfigured("127.0.0.1:"+port, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { link.Close() })
	local := availablePort()
	remote := local + ":" + strings.TrimPrefix(origin.URL, "http://")
	if reverse {
		remote = "R:" + remote
	}
	client, err := chclient.NewClient(&chclient.Config{Server: "http://" + link.Addr(), Fingerprint: server.GetFingerprint(), Remotes: []string{remote}, MaxRetryCount: -1, MinRetryInterval: 100 * time.Millisecond, MaxRetryInterval: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	client.SetOutput(io.Discard)
	if err = client.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); client.Wait() })
	ready, stop := context.WithTimeout(ctx, 5*time.Second)
	defer stop()
	if !client.Ready(ready) {
		t.Fatal("carrier did not become ready")
	}
	url := "http://127.0.0.1:" + local
	// SSH readiness can precede binding the reverse application's listener.
	// Wait for the actual data path before starting any measured workload.
	probe := &http.Client{Timeout: 500 * time.Millisecond}
	defer probe.CloseIdleConnections()
	for {
		if err = campaignEcho(ready, probe, url, []byte("application readiness")); err == nil {
			return url, link
		}
		select {
		case <-ready.Done():
			t.Fatalf("application path not ready: %v", err)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func campaignEcho(ctx context.Context, client *http.Client, url string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	r, err := client.Do(req)
	if err != nil {
		return err
	}
	defer r.Body.Close()
	got, err := io.ReadAll(io.LimitReader(r.Body, int64(len(body)+1)))
	if err != nil {
		return err
	}
	if r.StatusCode != 200 || !bytes.Equal(got, body) {
		return fmt.Errorf("incomplete/corrupt response: status %d length %d/%d", r.StatusCode, len(got), len(body))
	}
	return nil
}

func TestSharedCarrierRecoveryWithConcurrentFlows(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(fmt.Sprintf("reverse=%v", reverse), func(t *testing.T) {
			url, link := campaignTunnel(t, reverse, faultnet.Options{})
			client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}, Timeout: 2 * time.Second}
			defer client.CloseIdleConnections()
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			if err := campaignEcho(ctx, client, url, []byte("warm")); err != nil {
				t.Fatal(err)
			}
			before := link.Snapshot().Accepted
			link.SetMode(faultnet.Offline)
			const flows = 16
			start := make(chan struct{})
			results := make(chan error, flows)
			for flow := 0; flow < flows; flow++ {
				go func(id int) {
					<-start
					body := bytes.Repeat([]byte(fmt.Sprintf("flow-%02d|", id)), 512)
					for {
						err := campaignEcho(ctx, client, url, body)
						if err == nil {
							results <- nil
							return
						}
						select {
						case <-ctx.Done():
							results <- err
							return
						case <-time.After(10 * time.Millisecond):
						}
					}
				}(flow)
			}
			close(start)
			time.Sleep(250 * time.Millisecond)
			link.SetMode(faultnet.Online)
			for i := 0; i < flows; i++ {
				if err := <-results; err != nil {
					t.Error(err)
				}
			}
			got := link.Snapshot()
			if got.Accepted-before > 8 || got.Active != 1 {
				t.Fatalf("application flows multiplied carriers: before=%d after=%+v", before, got)
			}
			link.SetPause(false, true)
			request, cancelRequest := context.WithCancel(ctx)
			done := make(chan error, 1)
			go func() { done <- campaignEcho(request, client, url, []byte("cancel while response paused")) }()
			time.Sleep(20 * time.Millisecond)
			cancelRequest()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("paused response unexpectedly succeeded")
				}
			case <-time.After(time.Second):
				t.Fatal("cancellation blocked behind paused response")
			}
			link.SetPause(false, false)
			if err := campaignEcho(ctx, client, url, []byte("fresh flow after cancel")); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func BenchmarkTunnelHTTPMatrix(b *testing.B) {
	for _, impaired := range []bool{false, true} {
		for _, size := range []int{128, 64 << 10} {
			for _, flows := range []int{1, 8} {
				b.Run(fmt.Sprintf("weak=%v/bytes=%d/flows=%d", impaired, size, flows), func(b *testing.B) {
					options := faultnet.Options{}
					if impaired {
						options.Upload = faultnet.Traffic{Delay: 5 * time.Millisecond, BytesPerSecond: 256 << 10}
						options.Download = options.Upload
					}
					url, _ := campaignTunnel(b, false, options)
					client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{MaxIdleConnsPerHost: flows}}
					defer client.CloseIdleConnections()
					body := bytes.Repeat([]byte{'m'}, size)
					if err := campaignEcho(context.Background(), client, url, body); err != nil {
						b.Fatal(err)
					}
					samples := make([][]int64, flows)
					failures := make([]int, flows)
					for i := range samples {
						samples[i] = make([]int64, 0, 1024)
					}
					var workers sync.WaitGroup
					b.ReportAllocs()
					b.ResetTimer()
					for worker := 0; worker < flows; worker++ {
						workers.Add(1)
						go func(id int) {
							defer workers.Done()
							for i := id; i < b.N; i += flows {
								start := time.Now()
								err := campaignEcho(context.Background(), client, url, body)
								if err != nil {
									failures[id]++
								}
								// Sample successful AND failed attempt latency in a bounded ring.
								ms := time.Since(start).Microseconds()
								j := (i / flows) % 1024
								if j < len(samples[id]) {
									samples[id][j] = ms
								} else {
									samples[id] = append(samples[id], ms)
								}
							}
						}(worker)
					}
					workers.Wait()
					b.StopTimer()
					var all []int64
					failed := 0
					for i := range samples {
						all = append(all, samples[i]...)
						failed += failures[i]
					}
					slices.Sort(all)
					if len(all) > 0 {
						b.ReportMetric(float64(all[(len(all)-1)*95/100]), "p95-us")
						b.ReportMetric(float64(all[(len(all)-1)*99/100]), "p99-us")
					}
					b.ReportMetric(float64(b.N-failed), "completed")
					b.ReportMetric(float64(failed), "failed")
					b.ReportMetric(float64(b.N-failed)*float64(size)/b.Elapsed().Seconds()/1e6, "verified-MB/s")
					if failed != 0 {
						b.Errorf("%d/%d incomplete transfers", failed, b.N)
					}
				})
			}
		}
	}
}
