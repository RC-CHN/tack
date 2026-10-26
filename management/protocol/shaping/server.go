package shaping

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

// TLSListener accepts bounded-v1 and ordinary HTTP/1.1 on the same TLS port.
// Selection happens inside TLS, without a custom ALPN or another round trip.
// Handshakes run in net/http's connection worker, never in the accept loop.
func TLSListener(raw net.Listener, config *tls.Config) net.Listener {
	return &listener{Listener: raw, config: http1Config(config)}
}

func http1Config(config *tls.Config) *tls.Config {
	c := config.Clone()
	c.NextProtos = []string{"http/1.1"}
	for _, p := range config.NextProtos {
		if p == "acme-tls/1" {
			c.NextProtos = append(c.NextProtos, p)
		}
	}
	if get := c.GetConfigForClient; get != nil {
		c.GetConfigForClient = func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			next, err := get(hello)
			if err != nil || next == nil {
				return nil, err
			}
			return http1Config(next), nil
		}
	}
	return c
}

type listener struct {
	net.Listener
	config *tls.Config
}

func (l *listener) Accept() (net.Conn, error) {
	raw, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &serverConn{Conn: tls.Server(raw, l.config)}, nil
}

type serverConn struct {
	*tls.Conn
	once       sync.Once
	rmu        sync.Mutex
	stream     net.Conn
	err        error
	first      [1]byte
	plainFirst bool
}

func (c *serverConn) selectStream() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if c.err = c.Conn.HandshakeContext(ctx); c.err != nil {
		c.Close()
		return
	}
	// ReadHeaderTimeout, set by net/http, also bounds this selection.
	if _, c.err = io.ReadFull(c.Conn, c.first[:]); c.err != nil {
		c.Close()
		return
	}
	if c.first[0] != preface[0] {
		c.stream = c.Conn
		c.plainFirst = true
		return
	}
	version := preface
	if _, c.err = io.ReadFull(c.Conn, version[1:]); c.err != nil {
		c.Close()
		return
	}
	if version != preface {
		c.err = errors.New("unsupported shaping version")
		c.Close()
		return
	}
	framed := NewClient(c.Conn)
	framed.rprefix = false // Consumed during protocol selection.
	c.stream = framed
}
func (c *serverConn) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	c.rmu.Lock()
	defer c.rmu.Unlock()
	c.once.Do(c.selectStream)
	if c.err != nil {
		return 0, c.err
	}
	if c.plainFirst {
		c.plainFirst = false
		p[0] = c.first[0]
		return 1, nil
	}
	return c.stream.Read(p)
}
func (c *serverConn) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	c.once.Do(c.selectStream)
	if c.err != nil {
		return 0, c.err
	}
	return c.stream.Write(p)
}

type tlsContextKey struct{}

// ConfigureServer restores Request.TLS for the wrapped connection and bounds
// silent peers. Call before Serve; returned handler also works after Hijack.
func ConfigureServer(s *http.Server, handler http.Handler) http.Handler {
	previous := s.ConnContext
	s.ConnContext = func(ctx context.Context, c net.Conn) context.Context {
		if previous != nil {
			ctx = previous(ctx, c)
		}
		if framed, ok := c.(*serverConn); ok {
			return context.WithValue(ctx, tlsContextKey{}, framed.Conn)
		}
		return ctx
	}
	if s.ReadHeaderTimeout == 0 {
		s.ReadHeaderTimeout = 10 * time.Second
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c, ok := r.Context().Value(tlsContextKey{}).(*tls.Conn); ok {
			state := c.ConnectionState()
			r.TLS = &state
		}
		handler.ServeHTTP(w, r)
	})
}

// ListenAndServeTLS keeps the standard server path untouched when shaping is off.
func ListenAndServeTLS(s *http.Server, certFile, keyFile, name string) error {
	if err := Check(name); err != nil {
		return err
	}
	if !Enabled(name) {
		return s.ListenAndServeTLS(certFile, keyFile)
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return err
	}
	config := s.TLSConfig
	if config == nil {
		config = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	config = config.Clone()
	config.Certificates = []tls.Certificate{cert}
	listener, err := net.Listen("tcp", s.Addr)
	if err != nil {
		return err
	}
	defer listener.Close()
	handler := s.Handler
	if handler == nil {
		handler = http.DefaultServeMux
	}
	s.Handler = ConfigureServer(s, handler)
	return s.Serve(TLSListener(listener, config))
}
