package tunnel

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/jpillora/chisel/share/cio"
	"golang.org/x/crypto/ssh"
)

func TestDisconnectedWaitsReleaseGoroutines(t *testing.T) {
	for _, mode := range []string{"canceled", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("CHISEL_SSH_WAIT", "1ms")
			if mode == "canceled" {
				t.Setenv("CHISEL_SSH_WAIT", "1s")
			}
			tun := New(Config{Logger: cio.NewLogger("test")})
			defer tun.activatingConn.DoneAll()
			baseline := runtime.NumGoroutine()
			for i := 0; i < 100; i++ {
				ctx := context.Background()
				cancel := func() {}
				if mode == "canceled" {
					ctx, cancel = context.WithTimeout(ctx, 100*time.Microsecond)
				}
				if tun.Ready(ctx) {
					t.Fatal("disconnected tunnel reported ready")
				}
				cancel()
			}
			deadline := time.Now().Add(time.Second)
			for runtime.NumGoroutine() > baseline+2 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if after := runtime.NumGoroutine(); after > baseline+2 {
				t.Fatalf("disconnected waits retained goroutines: before=%d after=%d", baseline, after)
			}
		})
	}
}

func TestBindSSHOwnsCancellationWatcher(t *testing.T) {
	tun := New(Config{Logger: cio.NewLogger("test")})
	baseline := runtime.NumGoroutine()
	for i := 0; i < 100; i++ {
		conn := newMockSSHConn(false)
		conn.Close()
		reqs, chans := make(chan *ssh.Request), make(chan ssh.NewChannel)
		close(reqs)
		close(chans)
		// The caller may keep its parent context alive through reconnects.
		if err := tun.BindSSH(context.Background(), conn, reqs, chans); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(time.Second)
	for runtime.NumGoroutine() > baseline+2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > baseline+2 {
		t.Fatalf("connection watchers retained: %d -> %d", baseline, after)
	}
	t.Setenv("CHISEL_SSH_WAIT", "1s")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if tun.Ready(ctx) || ctx.Err() == nil {
		t.Fatal("previous connection cancellation released the next connection's readiness wait")
	}
}
