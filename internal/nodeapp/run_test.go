package nodeapp

import (
	"context"
	"encoding/base64"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	chserver "github.com/jpillora/chisel/server"
	p "tack.local/management/protocol"
)

func TestRejectInvalidTunnelBeforeStartingNode(t *testing.T) {
	base := p.TunnelConfig{
		Server: "https://localhost", Auth: "node:separate-tunnel-password",
		Fingerprint: base64.StdEncoding.EncodeToString(make([]byte, 32)), Remotes: []string{"12345:127.0.0.1:80"},
	}
	for name, change := range map[string]func(*p.TunnelConfig){
		"plaintext":      func(c *p.TunnelConfig) { c.Server = "http://example.com" },
		"no-auth":        func(c *p.TunnelConfig) { c.Auth = "" },
		"no-fingerprint": func(c *p.TunnelConfig) { c.Fingerprint = "" },
		"no-remotes":     func(c *p.TunnelConfig) { c.Remotes = nil },
		"stdio":          func(c *p.TunnelConfig) { c.Remotes = []string{"stdio:127.0.0.1:22"} },
		"bad-keepalive":  func(c *p.TunnelConfig) { v := -1; c.KeepAliveSeconds = &v },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := base
			change(&cfg)
			if _, err := newTunnel(cfg, false, false); err == nil {
				t.Fatal("invalid tunnel accepted")
			}
		})
	}
}

func availableAddress(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

func TestTunnelWorksWhileControllerOfflineAndReleasesListener(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	echoDone := make(chan struct{})
	go func() {
		defer close(echoDone)
		c, err := echo.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(5 * time.Second))
		io.Copy(c, c)
	}()
	srv, err := chserver.NewServer(&chserver.Config{Auth: "node:separate-password"})
	if err != nil {
		t.Fatal(err)
	}
	srv.Info = false
	srv.SetOutput(io.Discard)
	serverAddr := availableAddress(t)
	host, port, _ := net.SplitHostPort(serverAddr)
	if err := srv.StartContext(ctx, host, port); err != nil {
		t.Fatal(err)
	}
	defer func() { srv.Close(); srv.Wait() }()
	forward := availableAddress(t)
	zero := 0
	cfg := p.NodeConfig{NodeID: "unified-test", Token: strings.Repeat("x", 32),
		StateDir: t.TempDir(), WorkDir: t.TempDir(), Server: "http://127.0.0.1:1", AllowInsecureLoopback: true,
		Tunnel: &p.TunnelConfig{Server: "http://" + serverAddr, Auth: "node:separate-password",
			Fingerprint: srv.GetFingerprint(), Remotes: []string{forward + ":" + echo.Addr().String()}, KeepAliveSeconds: &zero},
	}
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg, false) }()
	var conn net.Conn
	for ctx.Err() == nil {
		conn, err = net.DialTimeout("tcp", forward, 100*time.Millisecond)
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	_, err = conn.Write([]byte("unified"))
	if err != nil {
		conn.Close()
		t.Fatal(err)
	}
	data := make([]byte, len("unified"))
	_, err = io.ReadFull(conn, data)
	conn.Close()
	if err != nil || string(data) != "unified" {
		t.Fatalf("tunnel while controller offline: %q %v", data, err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("combined shutdown blocked")
	}
	l, err := net.Listen("tcp", forward)
	if err != nil {
		t.Fatal("forward listener leaked:", err)
	}
	l.Close()
	<-echoDone
}
