package linklab

import (
	"bytes"
	"errors"
	"net"
	"testing"
	"time"
)

func udpEcho(t *testing.T) string {
	t.Helper()
	c, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	go func() {
		buf := make([]byte, 65535)
		for {
			n, a, err := c.ReadFrom(buf)
			if err != nil {
				return
			}
			c.WriteTo(buf[:n], a)
		}
	}()
	return c.LocalAddr().String()
}

func TestLiveUDPDelayLossChangesAndRecovery(t *testing.T) {
	u, err := NewUDP(udpEcho(t), Config{Paths: [2]Path{{Delay: 15 * time.Millisecond}, {Delay: 15 * time.Millisecond}}, Peers: 2}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()
	c, err := net.Dial("udp", u.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	probe := func(want bool) {
		t.Helper()
		c.SetDeadline(time.Now().Add(200 * time.Millisecond))
		start := time.Now()
		c.Write([]byte("datagram"))
		buf := make([]byte, 128)
		n, err := c.Read(buf)
		if want && (err != nil || !bytes.Equal(buf[:n], []byte("datagram")) || time.Since(start) < 30*time.Millisecond) {
			t.Fatal("delayed echo", err)
		}
		if !want && err == nil {
			t.Fatal("packet survived total loss")
		}
	}
	probe(true)
	if err = u.SetLoss(Download, 1, 0); err != nil {
		t.Fatal(err)
	}
	probe(false)
	if err = u.SetLoss(Download, 0, 0); err != nil {
		t.Fatal(err)
	}
	probe(true)
	u.Close()
	if s := u.Snapshot(); s.Peers != 0 || s.Model.QueuedBytes != 0 || s.Model.Lost == 0 {
		t.Fatal(s)
	}
}

func TestLiveUDPPeersExpireAndCloseDropsDelayedPayload(t *testing.T) {
	u, err := NewUDP(udpEcho(t), Config{Peers: 1, QueueBytes: 32, Paths: [2]Path{{Delay: time.Hour}, {}}}, 30*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()
	a, err := net.Dial("udp", u.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	a.Write([]byte("pending"))
	deadline := time.Now().Add(time.Second)
	for u.Snapshot().Model.Submitted == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if u.Snapshot().Model.Submitted == 0 {
		t.Fatal("fixture never received packet")
	}
	for u.Snapshot().Peers > 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if s := u.Snapshot(); s.Peers != 0 || s.Model.QueuedBytes != 0 {
		t.Fatal("expired peer retained payload", s)
	}
	a.Write([]byte("again"))
	done := make(chan struct{})
	go func() { u.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("delayed UDP prevented shutdown")
	}
	if err = u.SetLoss(Upload, 0, 0); !errors.Is(err, net.ErrClosed) {
		t.Fatal(err)
	}
	if datagramError(net.ErrClosed) {
		t.Fatal("closed socket was treated as datagram loss")
	}
}

func TestLiveUDPRecoversWhenDestinationDisappears(t *testing.T) {
	reserved, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	target := reserved.LocalAddr().String()
	reserved.Close()
	u, err := NewUDP(target, Config{Peers: 1}, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()
	c, err := net.Dial("udp", u.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(80 * time.Millisecond))
	c.Write([]byte("missing"))
	buf := make([]byte, 128)
	if _, err = c.Read(buf); err == nil {
		t.Fatal("closed destination replied")
	}
	server, err := net.ListenPacket("udp4", target)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	go func() {
		b := make([]byte, 128)
		for {
			n, a, err := server.ReadFrom(b)
			if err != nil {
				return
			}
			server.WriteTo(b[:n], a)
		}
	}()
	c.SetDeadline(time.Now().Add(time.Second))
	c.Write([]byte("returned"))
	n, err := c.Read(buf)
	if err != nil || string(buf[:n]) != "returned" {
		t.Fatal("datagram failure destroyed association", err)
	}
	if u.Snapshot().Peers != 1 {
		t.Fatal("unexpected peer churn")
	}
	u.Close()
	if len(u.incoming) != 0 {
		t.Fatal("closed fixture retained ingress payloads")
	}
}
