package faultnet

import (
	"bytes"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

func TestConfiguredStreamPreservesHalfCloseAndPayload(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	serverDone := make(chan error, 1)
	go func() {
		c, err := l.Accept()
		if err != nil {
			serverDone <- err
			return
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(4 * time.Second))
		data, err := io.ReadAll(io.LimitReader(c, 1<<20))
		if err == nil {
			_, err = c.Write(data)
		}
		serverDone <- err
	}()
	p, err := NewConfigured(l.Addr().String(), Options{Upload: Traffic{Delay: 10 * time.Millisecond}, Download: Traffic{Delay: 10 * time.Millisecond}, QueueChunks: 2, ChunkBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	c, err := net.Dial("tcp", p.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(4 * time.Second))
	data := bytes.Repeat([]byte("pipeline"), 32<<10)
	start := time.Now()
	if _, err = c.Write(data); err != nil {
		t.Fatal(err)
	}
	if err = c.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(c)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatal("corrupted/unfinished stream", len(got), err)
	}
	if elapsed := time.Since(start); elapsed < 20*time.Millisecond || elapsed > 3*time.Second {
		t.Fatal("delay missing or serialized per chunk without pipelining", elapsed)
	}
	if err = <-serverDone; err != nil {
		t.Fatal(err)
	}
}

func TestSlowRateDoesNotPreventClose(t *testing.T) {
	for _, profile := range []Traffic{{Delay: time.Hour}, {BytesPerSecond: 1}} {
		p, err := NewConfigured(echo(t), Options{Upload: profile, QueueChunks: 1, ChunkBytes: 64})
		if err != nil {
			t.Fatal(err)
		}
		c, err := net.Dial("tcp", p.Addr())
		if err != nil {
			p.Close()
			t.Fatal(err)
		}
		written := make(chan struct{})
		go func() { c.Write(make([]byte, 1<<20)); close(written) }()
		time.Sleep(15 * time.Millisecond)
		done := make(chan struct{})
		go func() { p.Close(); close(done) }()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("close blocked on pacing timer")
		}
		c.Close()
		<-written
		if p.Snapshot().Active != 0 {
			t.Fatal("close retained connections")
		}
	}
}

func TestRateAndDirectionalPause(t *testing.T) {
	p, err := NewConfigured(echo(t), Options{Upload: Traffic{BytesPerSecond: 8192}, ChunkBytes: 512})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	c, err := net.Dial("tcp", p.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	data := bytes.Repeat([]byte("a"), 2048)
	start := time.Now()
	if _, err = c.Write(data); err != nil {
		t.Fatal(err)
	}
	if _, err = io.ReadFull(c, make([]byte, len(data))); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < 240*time.Millisecond || elapsed > 2*time.Second {
		t.Fatal("incorrect serialization rate", elapsed)
	}
	p.SetPause(false, true)
	before := p.Snapshot().UpBytes
	c.Write([]byte("x"))
	c.SetReadDeadline(time.Now().Add(120 * time.Millisecond))
	if _, err = c.Read(make([]byte, 1)); err == nil {
		t.Fatal("paused download delivered bytes")
	}
	if p.Snapshot().UpBytes <= before {
		t.Fatal("download pause blocked upload too")
	}
	p.SetPause(false, false)
	c.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 1)
	if _, err = io.ReadFull(c, buf); err != nil || buf[0] != 'x' {
		t.Fatal("resume lost bytes", err)
	}
}

func TestConcurrentResetAndConnectionCeiling(t *testing.T) {
	p, err := NewConfigured(echo(t), Options{Connections: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	p.SetMode(Paused)
	conns := make([]net.Conn, 12)
	for i := range conns {
		conns[i], err = net.Dial("tcp", p.Addr())
		if err != nil {
			t.Fatal(err)
		}
		defer conns[i].Close()
		conns[i].Write([]byte("x"))
	}
	deadline := time.Now().Add(time.Second)
	for p.Snapshot().Accepted < 12 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if c := p.Snapshot(); c.Active > 2 || c.Rejected < 10 {
		t.Fatal("connection cap ignored", c)
	}
	var workers sync.WaitGroup
	for i := 0; i < 8; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for j := 0; j < 20; j++ {
				p.Reset()
				p.SetPause(j%2 == 0, j%2 != 0)
			}
		}()
	}
	workers.Wait()
	p.Close()
	if p.Snapshot().Active != 0 {
		t.Fatal("concurrent reset retained workers")
	}
}
