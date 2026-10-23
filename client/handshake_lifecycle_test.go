package chclient

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	chshare "github.com/jpillora/chisel/share"
)

func TestCancellationClosesSilentSSHHandshake(t *testing.T) {
	accepted := make(chan *websocket.Conn, 1)
	closed := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrade := websocket.Upgrader{Subprotocols: []string{chshare.ProtocolVersion}}
		conn, err := upgrade.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		defer close(closed)
		accepted <- conn
		// Drain the client's identification banner but never send an SSH banner.
		for {
			if _, _, err = conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer server.Close()
	client, err := NewClient(&Config{Server: server.URL, MaxRetryCount: -1})
	if err != nil {
		t.Fatal(err)
	}
	client.SetOutput(io.Discard)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := client.connectionOnce(ctx); done <- err }()
	var conn *websocket.Conn
	select {
	case conn = <-accepted:
	case <-time.After(time.Second):
		t.Fatal("WebSocket did not connect")
	}
	defer conn.Close()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		conn.Close()
		<-done
		t.Fatal("cancel did not interrupt silent SSH handshake")
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("handshake cancellation retained server socket")
	}
}
