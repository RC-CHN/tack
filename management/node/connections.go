package node

import (
	"context"
	"net"
	"net/http"
	"sync"
)

// The node exclusively owns these two transports. Canceling an HTTP/2 request
// may return before its stream becomes idle, so CloseIdleConnections alone does
// not guarantee prompt socket cleanup at shutdown.
type connectionSet struct {
	mu          sync.Mutex
	closed      bool
	context     context.Context
	cancel      context.CancelFunc
	dials       sync.WaitGroup
	connections map[*ownedConnection]struct{}
}
type ownedConnection struct {
	net.Conn
	owner *connectionSet
	once  sync.Once
	err   error
}

func newConnectionSet() *connectionSet {
	ctx, cancel := context.WithCancel(context.Background())
	return &connectionSet{context: ctx, cancel: cancel}
}

func (c *ownedConnection) Close() error {
	c.once.Do(func() {
		c.owner.mu.Lock()
		delete(c.owner.connections, c)
		c.owner.mu.Unlock()
		c.err = c.Conn.Close()
	})
	return c.err
}
func (s *connectionSet) attach(client *http.Client) {
	transport := client.Transport.(*http.Transport)
	dial := transport.DialContext
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return nil, net.ErrClosed
		}
		s.dials.Add(1)
		s.mu.Unlock()
		defer s.dials.Done()
		dialCtx, cancel := context.WithCancel(ctx)
		stop := context.AfterFunc(s.context, cancel)
		defer stop()
		defer cancel()
		conn, err := dial(dialCtx, network, address)
		if err != nil {
			return nil, err
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			conn.Close()
			return nil, net.ErrClosed
		}
		owned := &ownedConnection{Conn: conn, owner: s}
		if s.connections == nil {
			s.connections = make(map[*ownedConnection]struct{})
		}
		s.connections[owned] = struct{}{}
		s.mu.Unlock()
		return owned, nil
	}
}
func (s *connectionSet) isClosed() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.closed }
func (s *connectionSet) close() {
	s.mu.Lock()
	s.closed = true
	s.cancel()
	connections := make([]*ownedConnection, 0, len(s.connections))
	for conn := range s.connections {
		connections = append(connections, conn)
	}
	s.mu.Unlock()
	for _, conn := range connections {
		conn.Close()
	}
	s.dials.Wait()
}
