// Package faultnet provides a bounded TCP fault proxy for TLS, HTTP and SSH
// integration tests. It models outages and stalled peers, not IP packet loss.
// Its behavior specification is in ../BEHAVIOR.md.
package faultnet

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

type Mode int

type Traffic struct {
	Delay          time.Duration
	BytesPerSecond int64
}

type Options struct {
	Upload, Download                     Traffic
	Connections, QueueChunks, ChunkBytes int
}

const (
	Online  Mode = iota
	Paused       // Stop forwarding both directions without discarding stream bytes.
	Offline      // Close current connections and reject new ones.
)

type Stats struct {
	Accepted  uint64 `json:"accepted_connections"`
	Rejected  uint64 `json:"rejected_connections"`
	Active    int    `json:"active_connections"`
	UpBytes   uint64 `json:"upstream_stream_bytes"`
	DownBytes uint64 `json:"downstream_stream_bytes"`
}

type pair struct {
	client, server net.Conn
	ctx            context.Context
	cancel         context.CancelFunc
}
type Proxy struct {
	listener                     net.Listener
	target                       string
	ctx                          context.Context
	cancel                       context.CancelFunc
	mu                           sync.Mutex
	mode                         Mode
	paused                       [2]bool
	options                      Options
	changed                      chan struct{}
	closed                       bool
	pairs                        map[*pair]struct{}
	wg                           sync.WaitGroup
	once                         sync.Once
	accepted, rejected, up, down atomic.Uint64
}

func New(target string) (*Proxy, error) {
	return NewConfigured(target, Options{})
}

func NewConfigured(target string, options Options) (*Proxy, error) {
	if options.Connections == 0 {
		options.Connections = 16
	}
	if options.QueueChunks == 0 {
		options.QueueChunks = 4
	}
	if options.ChunkBytes == 0 {
		options.ChunkBytes = 16 * 1024
	}
	if options.Connections < 1 || options.Connections > 256 || options.QueueChunks < 1 || options.QueueChunks > 64 || options.ChunkBytes < 1 || options.ChunkBytes > 65536 {
		return nil, errors.New("invalid TCP fixture resource bounds")
	}
	for _, profile := range []Traffic{options.Upload, options.Download} {
		if profile.Delay < 0 || profile.Delay > time.Hour || profile.BytesPerSecond < 0 {
			return nil, errors.New("invalid TCP delay or rate")
		}
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &Proxy{listener: l, target: target, ctx: ctx, cancel: cancel, changed: make(chan struct{}), pairs: make(map[*pair]struct{}), options: options}
	p.wg.Add(1)
	go p.accept()
	return p, nil
}

func (p *Proxy) Addr() string { return p.listener.Addr().String() }
func (p *Proxy) Snapshot() Stats {
	p.mu.Lock()
	n := len(p.pairs)
	p.mu.Unlock()
	return Stats{p.accepted.Load(), p.rejected.Load(), n, p.up.Load(), p.down.Load()}
}

func (p *Proxy) SetMode(mode Mode) {
	if mode < Online || mode > Offline {
		panic("invalid fault mode")
	}
	p.mu.Lock()
	p.mode = mode
	close(p.changed)
	p.changed = make(chan struct{})
	if mode == Offline {
		for c := range p.pairs {
			c.cancel()
			c.client.Close()
			if c.server != nil {
				c.server.Close()
			}
		}
	}
	p.mu.Unlock()
}

// SetPause can stall just one direction, to expose control/data coupling.
func (p *Proxy) SetPause(upload, download bool) {
	p.mu.Lock()
	p.paused = [2]bool{upload, download}
	close(p.changed)
	p.changed = make(chan struct{})
	p.mu.Unlock()
}

// Reset tears down current connections while leaving new connections enabled.
func (p *Proxy) Reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for c := range p.pairs {
		c.cancel()
		c.client.Close()
		if c.server != nil {
			c.server.Close()
		}
	}
}

func (p *Proxy) Close() error {
	p.once.Do(func() {
		p.mu.Lock()
		p.closed = true
		p.cancel()
		p.listener.Close()
		for c := range p.pairs {
			c.cancel()
			c.client.Close()
			if c.server != nil {
				c.server.Close()
			}
		}
		p.mu.Unlock()
		p.wg.Wait()
	})
	return nil
}

func (p *Proxy) accept() {
	defer p.wg.Done()
	for {
		c, err := p.listener.Accept()
		if err != nil {
			return
		}
		p.accepted.Add(1)
		p.mu.Lock()
		if p.closed || p.mode == Offline || len(p.pairs) >= p.options.Connections {
			p.mu.Unlock()
			p.rejected.Add(1)
			c.Close()
			continue
		}
		ctx, cancel := context.WithCancel(p.ctx)
		conn := &pair{client: c, ctx: ctx, cancel: cancel}
		p.pairs[conn] = struct{}{}
		p.wg.Add(1)
		p.mu.Unlock()
		go p.serve(conn)
	}
}

func (p *Proxy) serve(c *pair) {
	defer p.wg.Done()
	defer func() {
		c.cancel()
		p.mu.Lock()
		delete(p.pairs, c)
		c.client.Close()
		if c.server != nil {
			c.server.Close()
		}
		p.mu.Unlock()
	}()
	d := net.Dialer{Timeout: time.Second}
	s, err := d.DialContext(c.ctx, "tcp", p.target)
	if err != nil {
		return
	}
	p.mu.Lock()
	c.server = s
	stopped := p.closed || p.mode == Offline
	p.mu.Unlock()
	if stopped {
		return
	}
	ctx, cancel := context.WithCancel(c.ctx)
	defer cancel()
	var pumps sync.WaitGroup
	pumps.Add(2)
	pump := func(from, to net.Conn, count *atomic.Uint64, direction int) {
		defer pumps.Done()
		err := p.copy(ctx, from, to, count, direction)
		if err != nil {
			cancel()
			from.Close()
			to.Close()
			return
		}
		if cw, ok := to.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		}
	}
	go pump(c.client, s, &p.up, 0)
	go pump(s, c.client, &p.down, 1)
	pumps.Wait()
}

func (p *Proxy) ready(ctx context.Context, direction int) error {
	for {
		p.mu.Lock()
		mode, changed, closed, paused := p.mode, p.changed, p.closed, p.paused[direction]
		p.mu.Unlock()
		if closed || mode == Offline {
			return net.ErrClosed
		}
		if mode == Online && !paused {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}

func wait(ctx context.Context, duration time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if duration <= 0 {
		return nil
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *Proxy) copy(ctx context.Context, from, to net.Conn, count *atomic.Uint64, direction int) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type chunk struct {
		bytes []byte
		due   time.Time
	}
	queue := make(chan chunk, p.options.QueueChunks)
	// Fixed buffers bound resident payload independently of stream length.
	free := make(chan []byte, p.options.QueueChunks+2)
	for i := 0; i < cap(free); i++ {
		free <- make([]byte, p.options.ChunkBytes)
	}
	done := make(chan error, 1)
	profile := [2]Traffic{p.options.Upload, p.options.Download}[direction]
	go func() {
		var result error
		defer func() {
			if result != nil {
				cancel()
				from.Close()
				to.Close()
			}
			done <- result
		}()
		next := time.Now()
		for c := range queue {
			due := c.due
			if profile.BytesPerSecond > 0 {
				if next.Before(time.Now()) {
					next = time.Now()
				}
				next = next.Add(time.Duration(float64(len(c.bytes)) * float64(time.Second) / float64(profile.BytesPerSecond)))
				if next.After(due) {
					due = next
				}
			}
			if result = wait(ctx, time.Until(due)); result != nil {
				return
			}
			if result = p.ready(ctx, direction); result != nil {
				return
			}
			n, err := to.Write(c.bytes)
			count.Add(uint64(n))
			if err != nil {
				result = err
				return
			}
			if n != len(c.bytes) {
				result = io.ErrShortWrite
				return
			}
			free <- c.bytes[:cap(c.bytes)]
		}
	}()
	var readErr error
readLoop:
	for {
		var buf []byte
		select {
		case buf = <-free:
		case <-ctx.Done():
			readErr = ctx.Err()
			break readLoop
		}
		n, err := from.Read(buf)
		if n > 0 {
			select {
			case queue <- chunk{buf[:n], time.Now().Add(profile.Delay)}:
			case <-ctx.Done():
				readErr = ctx.Err()
				break readLoop
			}
		} else {
			free <- buf
		}
		if err != nil {
			if err != io.EOF {
				readErr = err
				cancel()
			}
			break
		}
	}
	close(queue)
	if err := <-done; err != nil {
		return err
	}
	return readErr
}
