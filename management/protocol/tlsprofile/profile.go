// Package tlsprofile keeps optional TLS implementations out of Legacy builds.
// Providers register at init time; the default remains the Go standard library.
package tlsprofile

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sync"
	"tack.local/management/protocol/shaping"
)

type DialFunc func(context.Context, string, string) (net.Conn, error)
type HandshakeFunc func(context.Context, net.Conn, *tls.Config) (net.Conn, error)
type IdentityHandshakeFunc func(context.Context, net.Conn, *tls.Config, [32]byte) (net.Conn, error)

var providers = struct {
	sync.RWMutex
	items      map[string]HandshakeFunc
	identities map[string]IdentityHandshakeFunc
}{items: make(map[string]HandshakeFunc), identities: make(map[string]IdentityHandshakeFunc)}

func Register(name string, handshake HandshakeFunc) {
	providers.Lock()
	defer providers.Unlock()
	if Native(name) || handshake == nil || providers.items[name] != nil || providers.identities[name] != nil {
		panic("invalid or duplicate TLS profile registration")
	}
	providers.items[name] = handshake
}

// RegisterIdentity registers a profile whose template is derived from a client
// identity. The seed must never replace TLS handshake or key-generation entropy.
func RegisterIdentity(name string, handshake IdentityHandshakeFunc) {
	providers.Lock()
	defer providers.Unlock()
	if Native(name) || handshake == nil || providers.items[name] != nil || providers.identities[name] != nil {
		panic("invalid or duplicate TLS profile registration")
	}
	providers.identities[name] = handshake
}

func NeedsIdentity(name string) bool {
	providers.RLock()
	defer providers.RUnlock()
	return providers.identities[name] != nil
}

func Native(name string) bool { return name == "" || name == "native" }

func Check(name string) error {
	if Native(name) {
		return nil
	}
	providers.RLock()
	defer providers.RUnlock()
	if providers.items[name] == nil && providers.identities[name] == nil {
		return fmt.Errorf("TLS profile %q is unavailable in this build", name)
	}
	return nil
}

// DialTLS preserves the caller's raw dialer (including connection ownership),
// validates certificates, and closes the raw socket on every handshake failure.
func DialTLS(name string, dial DialFunc, config *tls.Config, identities ...*Identity) (DialFunc, error) {
	if err := Check(name); err != nil {
		return nil, err
	}
	if config == nil || config.InsecureSkipVerify {
		return nil, errors.New("custom TLS profile requires certificate verification")
	}
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	providers.RLock()
	handshake := providers.items[name]
	personalized := providers.identities[name]
	providers.RUnlock()
	if len(identities) > 1 {
		return nil, errors.New("at most one TLS identity is allowed")
	}
	var identity *Identity
	if len(identities) == 1 {
		identity = identities[0]
	}
	if personalized != nil {
		if identity == nil {
			return nil, errors.New("TLS profile requires a persistent client identity")
		}
		seed := identity.seed
		handshake = func(ctx context.Context, raw net.Conn, cfg *tls.Config) (net.Conn, error) {
			return personalized(ctx, raw, cfg, seed)
		}
	} else if identity != nil {
		return nil, errors.New("TLS identity is not used by this profile")
	}
	if Native(name) {
		handshake = func(ctx context.Context, raw net.Conn, cfg *tls.Config) (net.Conn, error) {
			c := tls.Client(raw, cfg)
			if err := c.HandshakeContext(ctx); err != nil {
				return nil, err
			}
			if alpn := c.ConnectionState().NegotiatedProtocol; alpn != "" && alpn != "http/1.1" {
				return nil, errors.New("unexpected ALPN for shaped HTTP/1.1")
			}
			return c, nil
		}
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		cfg := config.Clone()
		if cfg.ServerName == "" {
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			cfg.ServerName = host
		}
		raw, err := dial(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		conn, err := handshake(ctx, raw, cfg)
		if err != nil {
			raw.Close()
			return nil, err
		}
		return conn, nil
	}, nil
}

// ConfigureHTTP installs the HTTP/1.1 profile after the caller has wrapped
// DialContext. CONNECT paths in net/http bypass DialTLSContext, so reject proxy
// use explicitly instead of silently sending a native TLS fingerprint.
func ConfigureHTTP(t *http.Transport, name string, identities ...*Identity) error {
	return ConfigureTransport(t, name, "", identities...)
}

// DialTransport composes a verified TLS handshake and optional shaping. It
// returns before application data, so the version prefix adds no handshake RTT.
func DialTransport(name, shape string, dial DialFunc, config *tls.Config, identities ...*Identity) (DialFunc, error) {
	if err := shaping.Check(shape); err != nil {
		return nil, err
	}
	if config == nil {
		return nil, errors.New("verified TLS configuration required")
	}
	config = config.Clone()
	config.NextProtos = []string{"http/1.1"}
	tlsDial, err := DialTLS(name, dial, config, identities...)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := tlsDial(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		if shaping.Enabled(shape) {
			return shaping.NewClient(conn), nil
		}
		return conn, nil
	}, nil
}

func ConfigureTransport(t *http.Transport, name, shape string, identities ...*Identity) error {
	if err := shaping.Check(shape); err != nil {
		return err
	}
	if Native(name) && !shaping.Enabled(shape) {
		if len(identities) > 1 || (len(identities) == 1 && identities[0] != nil) {
			return errors.New("TLS identity is not used by this profile")
		}
		return nil
	}
	dial, err := DialTransport(name, shape, t.DialContext, t.TLSClientConfig, identities...)
	if err != nil {
		return err
	}
	t.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if t.TLSHandshakeTimeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, t.TLSHandshakeTimeout)
			defer cancel()
		}
		return dial(ctx, network, addr)
	}
	proxy := t.Proxy
	if proxy != nil {
		t.Proxy = func(r *http.Request) (*url.URL, error) {
			p, err := proxy(r)
			if err == nil && p != nil {
				err = errors.New("TLS profiles and shaping do not support HTTP transport proxies")
			}
			return nil, err
		}
	}
	t.ForceAttemptHTTP2 = false
	t.TLSNextProto = make(map[string]func(string, *tls.Conn) http.RoundTripper)
	return nil
}
