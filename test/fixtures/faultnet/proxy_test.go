package faultnet

import (
	"io"
	"net"
	"testing"
	"time"
)

func echo(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, e := l.Accept()
			if e != nil {
				return
			}
			go func() { defer c.Close(); io.Copy(c, c) }()
		}
	}()
	return l.Addr().String()
}

func TestPauseResumeResetAndShutdown(t *testing.T) {
	p, err := New(echo(t))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	for i := 0; i < 24; i++ {
		c, err := net.DialTimeout("tcp", p.Addr(), time.Second)
		if err != nil {
			t.Fatal(err)
		}
		c.SetDeadline(time.Now().Add(2 * time.Second))
		if _, err = c.Write([]byte("a")); err != nil {
			t.Fatal(err)
		}
		b := make([]byte, 1)
		if _, err = io.ReadFull(c, b); err != nil || b[0] != 'a' {
			t.Fatal("initial echo", err)
		}
		p.SetMode(Paused)
		c.Write([]byte("b"))
		c.SetReadDeadline(time.Now().Add(15 * time.Millisecond))
		if _, err = c.Read(b); err == nil {
			t.Fatal("pause forwarded bytes")
		}
		c.SetReadDeadline(time.Now().Add(time.Second))
		p.SetMode(Online)
		if _, err = io.ReadFull(c, b); err != nil || b[0] != 'b' {
			t.Fatal("resume corrupted stream", err)
		}
		p.SetMode(Paused)
		c.Write([]byte("c"))
		p.Reset()
		c.Close()
		p.SetMode(Online)
	}
	p.Close()
	if got := p.Snapshot(); got.Active != 0 || got.Accepted != 24 || got.UpBytes < 48 {
		t.Fatalf("leaked or unexercised: %+v", got)
	}
}

func TestOfflineRejectsAndCloseInterruptsPause(t *testing.T) {
	p, err := New(echo(t))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	p.SetMode(Offline)
	c, err := net.DialTimeout("tcp", p.Addr(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c.SetDeadline(time.Now().Add(time.Second))
	c.Write([]byte("x"))
	if _, err = c.Read(make([]byte, 1)); err == nil {
		t.Fatal("offline peer responded")
	}
	c.Close()
	p.SetMode(Paused)
	c, err = net.DialTimeout("tcp", p.Addr(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte("x"))
	done := make(chan struct{})
	go func() { p.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("paused workers survived Close")
	}
	if p.Snapshot().Rejected < 1 || p.Snapshot().Active != 0 {
		t.Fatal(p.Snapshot())
	}
}
