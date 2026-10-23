package chclient

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	chserver "github.com/jpillora/chisel/server"
	chshare "github.com/jpillora/chisel/share"
	"github.com/jpillora/chisel/share/cnet"
	"golang.org/x/crypto/ssh"
)

func stalledHandshakeServer(t testing.TB, configStage bool) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	config := &ssh.ServerConfig{NoClientAuth: true}
	config.AddHostKey(signer)
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := (&websocket.Upgrader{Subprotocols: []string{chshare.ProtocolVersion}}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		attempts.Add(1)
		if !configStage {
			for {
				if _, _, err := ws.ReadMessage(); err != nil {
					return
				}
			}
		}
		conn, _, requests, err := ssh.NewServerConn(cnet.NewWebSocketConn(ws), config)
		if err != nil {
			return
		}
		defer conn.Close()
		for range requests { /* Receive config but deliberately never acknowledge. */
		}
	}))
	t.Cleanup(server.Close)
	return server, &attempts
}

func TestHandshakeTimeoutRetriesSilentPeer(t *testing.T) {
	for _, configStage := range []bool{false, true} {
		name := "ssh_banner"
		if configStage {
			name = "config_ack"
		}
		t.Run(name, func(t *testing.T) {
			server, attempts := stalledHandshakeServer(t, configStage)
			client, err := NewClient(&Config{Server: server.URL, MaxRetryCount: 1, MinRetryInterval: 20 * time.Millisecond, MaxRetryInterval: 20 * time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			client.SetOutput(io.Discard)
			client.sshConfig.Timeout = 100 * time.Millisecond
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			start := time.Now()
			err = client.connectionLoop(ctx)
			if err == nil || ctx.Err() != nil || attempts.Load() != 2 {
				t.Fatalf("stalled handshake did not retry: %v context=%v attempts=%d", err, ctx.Err(), attempts.Load())
			}
			t.Logf("stage=%s attempts=%d elapsed=%s", name, attempts.Load(), time.Since(start))
		})
	}
}

// Before the fix, only the outer 500 ms watchdog terminates the attempt. Count
// that explicitly instead of describing it as a successful handshake timeout.
func BenchmarkStalledHandshake(b *testing.B) {
	server, _ := stalledHandshakeServer(b, false)
	client, err := NewClient(&Config{Server: server.URL, MaxRetryCount: 0})
	if err != nil {
		b.Fatal(err)
	}
	client.SetOutput(io.Discard)
	client.sshConfig.Timeout = 100 * time.Millisecond
	watchdogs := 0
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		_, err := client.connectionOnce(ctx)
		if ctx.Err() != nil {
			watchdogs++
		}
		cancel()
		if err == nil {
			b.Fatal("silent peer connected")
		}
	}
	b.ReportMetric(float64(watchdogs), "watchdog-expirations")
}

func TestHandshakeTimerDoesNotExpireEstablishedTunnel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server, err := chserver.NewServer(&chserver.Config{})
	if err != nil {
		t.Fatal(err)
	}
	server.SetOutput(io.Discard)
	port := availableTCPPort(t)
	if err := server.StartContext(ctx, "127.0.0.1", port); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); server.Wait() }()
	client, err := NewClient(&Config{Server: "http://127.0.0.1:" + port, Fingerprint: server.GetFingerprint(), MaxRetryCount: 0})
	if err != nil {
		t.Fatal(err)
	}
	client.SetOutput(io.Discard)
	client.sshConfig.Timeout = 100 * time.Millisecond
	if err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var waitErr error
	go func() { waitErr = client.Wait(); close(done) }()
	defer func() { cancel(); <-done }()
	ready, stop := context.WithTimeout(ctx, time.Second)
	defer stop()
	if !client.Ready(ready) {
		t.Fatal("tunnel never connected")
	}
	select {
	case <-done:
		t.Fatal("handshake timer closed established connection", waitErr)
	case <-time.After(350 * time.Millisecond):
	}
	if !client.Ready(ready) {
		t.Fatal("established tunnel lost")
	}
}
