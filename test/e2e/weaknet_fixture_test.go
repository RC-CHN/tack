package e2e_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	chclient "github.com/jpillora/chisel/client"
	chserver "github.com/jpillora/chisel/server"
	"tack.local/testfixtures/faultnet"
)

func TestTunnelRecoversAcrossImpairedTransport(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		name := "forward"
		if reverse {
			name = "reverse"
		}
		t.Run(name, func(t *testing.T) {
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.Copy(w, r.Body) }))
			defer origin.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			server, err := chserver.NewServer(&chserver.Config{Reverse: reverse})
			if err != nil {
				t.Fatal(err)
			}
			server.SetOutput(io.Discard)
			port := availablePort()
			if err = server.StartContext(ctx, "127.0.0.1", port); err != nil {
				t.Fatal(err)
			}
			defer func() { cancel(); server.Wait() }()
			relay, err := faultnet.NewConfigured("127.0.0.1:"+port, faultnet.Options{Upload: faultnet.Traffic{Delay: 25 * time.Millisecond, BytesPerSecond: 128 * 1024}, Download: faultnet.Traffic{Delay: 25 * time.Millisecond, BytesPerSecond: 128 * 1024}, QueueChunks: 2})
			if err != nil {
				t.Fatal(err)
			}
			defer relay.Close()
			link, err := faultnet.New(relay.Addr())
			if err != nil {
				t.Fatal(err)
			}
			defer link.Close()
			local := availablePort()
			remote := local + ":" + strings.TrimPrefix(origin.URL, "http://")
			if reverse {
				remote = "R:" + remote
			}
			client, err := chclient.NewClient(&chclient.Config{Server: "http://" + link.Addr(), Fingerprint: server.GetFingerprint(), Remotes: []string{remote}, MaxRetryCount: -1, MinRetryInterval: 50 * time.Millisecond, MaxRetryInterval: 200 * time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			client.SetOutput(io.Discard)
			if err = client.Start(ctx); err != nil {
				t.Fatal(err)
			}
			defer func() { cancel(); client.Wait() }()
			httpClient := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
			defer httpClient.CloseIdleConnections()
			probe := func() bool {
				r, err := httpClient.Post("http://127.0.0.1:"+local, "application/octet-stream", strings.NewReader("bounded echo"))
				if err != nil {
					return false
				}
				defer r.Body.Close()
				b, err := io.ReadAll(io.LimitReader(r.Body, 128))
				return err == nil && string(b) == "bounded echo"
			}
			wait := func() {
				t.Helper()
				deadline := time.Now().Add(12 * time.Second)
				for time.Now().Before(deadline) {
					if probe() {
						return
					}
					time.Sleep(20 * time.Millisecond)
				}
				t.Fatal("tunnel did not recover")
			}
			wait()
			for i := 0; i < 4; i++ {
				link.SetMode(faultnet.Offline)
				time.Sleep(100 * time.Millisecond)
				link.SetMode(faultnet.Online)
				wait()
			}
			link.SetMode(faultnet.Paused)
			// Queue real application work behind the paused connection.
			c, err := net.DialTimeout("tcp", "127.0.0.1:"+local, time.Second)
			if err == nil {
				c.Write([]byte("GET / HTTP/1.0\r\n\r\n"))
				defer c.Close()
			}
			start := time.Now()
			cancel()
			client.Wait()
			server.Wait()
			if time.Since(start) > 3*time.Second {
				t.Fatal("paused transport prevented shutdown")
			}
			link.Close()
			if got := link.Snapshot(); got.Active != 0 || got.Accepted > 40 {
				t.Fatalf("recovery churn or leaked connection: %+v", got)
			}
		})
	}
}
