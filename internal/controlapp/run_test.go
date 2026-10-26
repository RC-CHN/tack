//go:build go1.25

package controlapp

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	chshare "github.com/jpillora/chisel/share"
	"github.com/jpillora/chisel/share/cnet"
	"golang.org/x/crypto/ssh"
	"tack.local/management/control/command"
	p "tack.local/management/protocol"
)

func address(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}
func fixture(t *testing.T) (p.ControlConfig, command.Options) {
	t.Helper()
	dir := t.TempDir()
	key, auth := filepath.Join(dir, "ssh.key"), filepath.Join(dir, "users.json")
	if err := GenerateKey(key); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(auth, []byte(`{"node:tunnel-password":["^127\\.0\\.0\\.1:80$"]}`), 0600); err != nil {
		t.Fatal(err)
	}
	return p.ControlConfig{AdminToken: strings.Repeat("a", 32), Tunnel: &p.TunnelServerConfig{Listen: address(t), KeyFile: key, AuthFile: auth}}, command.Options{State: filepath.Join(dir, "state.json"), Listen: address(t), AdminListen: address(t)}
}
func mustReleased(t *testing.T, addresses ...string) {
	t.Helper()
	for _, addr := range addresses {
		l, err := net.Listen("tcp", addr)
		if err != nil {
			t.Errorf("listener not released %s: %v", addr, err)
		} else {
			l.Close()
		}
	}
}
func waitHTTP(t *testing.T, addr string) {
	t.Helper()
	client := &http.Client{Timeout: 100 * time.Millisecond}
	defer client.CloseIdleConnections()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); {
		resp, err := client.Get("http://" + addr + "/health")
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("listener never became ready")
}
func TestKeyGenerationDoesNotReplaceExistingIdentity(t *testing.T) {
	config, _ := fixture(t)
	before, err := os.ReadFile(config.Tunnel.KeyFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := GenerateKey(config.Tunnel.KeyFile); err == nil {
		t.Fatal("replaced existing key")
	}
	after, _ := os.ReadFile(config.Tunnel.KeyFile)
	if string(after) != string(before) {
		t.Fatal("key changed")
	}
	var original string
	for i := 0; i < 2; i++ {
		service, err := NewTunnel(*config.Tunnel, false)
		if err != nil {
			t.Fatal(err)
		}
		fp := service.Fingerprint()
		service.Close()
		if fp == "" {
			t.Fatal("empty fingerprint")
		}
		if i == 0 {
			original = fp
		} else if fp != original {
			t.Fatal("fingerprint changed")
		}
	}
}
func TestStartupFailureReleasesAllListeners(t *testing.T) {
	for _, kind := range []string{"invalid-auth", "invalid-tls", "tunnel-occupied", "admin-occupied", "management-only-entrypoint"} {
		t.Run(kind, func(t *testing.T) {
			config, options := fixture(t)
			support := Support()
			var occupied net.Listener
			switch kind {
			case "invalid-auth":
				os.WriteFile(config.Tunnel.AuthFile, []byte(`{`), 0600)
			case "invalid-tls":
				config.Tunnel.TLSCert = config.Tunnel.KeyFile
				config.Tunnel.TLSKey = config.Tunnel.KeyFile
			case "tunnel-occupied":
				occupied, _ = net.Listen("tcp", config.Tunnel.Listen)
			case "admin-occupied":
				occupied, _ = net.Listen("tcp", options.AdminListen)
			case "management-only-entrypoint":
				support = command.TunnelSupport{}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := command.Run(ctx, config, options, support); err == nil {
				t.Fatal("bad startup accepted")
			}
			if occupied != nil {
				occupied.Close()
			}
			mustReleased(t, options.Listen, options.AdminListen, config.Tunnel.Listen)
		})
	}
}
func TestShutdownClosesStalledSSHAndAllowsStateReuse(t *testing.T) {
	config, options := fixture(t)
	for i := 0; i < 2; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- command.Run(ctx, config, options, Support()) }()
		waitHTTP(t, config.Tunnel.Listen)
		dialer := websocket.Dialer{Subprotocols: []string{chshare.ProtocolVersion}, HandshakeTimeout: time.Second}
		conn, _, err := dialer.Dial("ws://"+config.Tunnel.Listen, nil)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		// Never send an SSH banner: shutdown must still close this hijacked socket.
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("shutdown blocked on SSH handshake")
		}
		conn.SetReadDeadline(time.Now().Add(time.Second))
		for {
			_, _, err = conn.ReadMessage()
			if err != nil {
				break
			}
		}
		if ne, ok := err.(net.Error); ok && ne.Timeout() {
			t.Fatal("hijacked connection remained open")
		}
		conn.Close()
		mustReleased(t, options.Listen, options.AdminListen, config.Tunnel.Listen)
	}
}
func TestTunnelAuthIndependentAndEmptyACLRejects(t *testing.T) {
	config, options := fixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- command.Run(ctx, config, options, Support()) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	waitHTTP(t, config.Tunnel.Listen)
	connect := func(user, password string) error {
		dialer := websocket.Dialer{Subprotocols: []string{chshare.ProtocolVersion}, HandshakeTimeout: time.Second}
		ws, _, err := dialer.Dial("ws://"+config.Tunnel.Listen, nil)
		if err != nil {
			return err
		}
		defer ws.Close()
		conn := cnet.NewWebSocketConn(ws)
		conn.SetDeadline(time.Now().Add(time.Second))
		sshConn, _, _, err := ssh.NewClientConn(conn, config.Tunnel.Listen, &ssh.ClientConfig{User: user, Auth: []ssh.AuthMethod{ssh.Password(password)}, HostKeyCallback: ssh.InsecureIgnoreHostKey()})
		if err == nil {
			sshConn.Close()
		}
		return err
	}
	if err := connect("node", "tunnel-password"); err != nil {
		t.Fatal(err)
	}
	if err := connect("node", config.AdminToken); err == nil {
		t.Fatal("management credential accepted as tunnel credential")
	}
	if err := os.WriteFile(config.Tunnel.AuthFile, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for connect("node", "tunnel-password") == nil {
		if time.Now().After(deadline) {
			t.Fatal("cleared authfile still authenticates")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := connect("anyone", "anything"); err == nil {
		t.Fatal("empty authfile enabled anonymous access")
	}
}
func TestOldConfigurationStillServesManagement(t *testing.T) {
	config, options := fixture(t)
	config.Tunnel = nil
	raw, _ := json.Marshal(config)
	var decoded p.ControlConfig
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- command.Run(ctx, decoded, options, command.TunnelSupport{}) }()
	waitHTTP(t, options.Listen)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	mustReleased(t, options.Listen, options.AdminListen)
}
