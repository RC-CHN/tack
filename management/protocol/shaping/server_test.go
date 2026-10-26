package shaping_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tack.local/management/protocol/shaping"
	"tack.local/management/protocol/tlsprofile"
)

func TestHTTPCompatibilityVerificationAndOwnership(t *testing.T) {
	seed := httptest.NewTLSServer(nil)
	defer seed.Close()
	pool := x509.NewCertPool()
	pool.AddCert(seed.Certificate())
	var accepts atomic.Int32
	s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || !r.TLS.HandshakeComplete || r.TLS.NegotiatedProtocol != "http/1.1" {
			t.Error("TLS state/ALPN lost", r.TLS)
		}
		if r.ProtoMajor != 1 {
			t.Error("not HTTP/1.1")
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		w.Write(body)
	}))
	s.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			accepts.Add(1)
		}
	}
	s.Config.Handler = shaping.ConfigureServer(s.Config, s.Config.Handler)
	s.Listener = shaping.TLSListener(s.Listener, seed.TLS)
	s.Start()
	defer s.Close()
	url := strings.Replace(s.URL, "http://", "https://", 1)
	for _, shape := range []string{"off", shaping.Profile} {
		t.Run(shape, func(t *testing.T) {
			tr := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, NextProtos: []string{"http/1.1"}}, TLSHandshakeTimeout: time.Second, ForceAttemptHTTP2: true}
			if err := tlsprofile.ConfigureTransport(tr, "native", shape); err != nil {
				t.Fatal(err)
			}
			defer tr.CloseIdleConnections()
			client := &http.Client{Transport: tr, Timeout: 2 * time.Second}
			before := accepts.Load()
			for i, size := range []int{1, 17, 32768} {
				want := bytes.Repeat([]byte{byte(i)}, size)
				r, err := client.Post(url, "application/octet-stream", bytes.NewReader(want))
				if err != nil {
					t.Fatal(err)
				}
				got, err := io.ReadAll(r.Body)
				r.Body.Close()
				if err != nil || !bytes.Equal(want, got) {
					t.Fatalf("corrupt echo status=%d length=%d want=%d body=%q err=%v", r.StatusCode, len(got), len(want), got[:min(128, len(got))], err)
				}
			}
			if accepts.Load()-before != 1 {
				t.Fatal("connection reuse lost")
			}
		})
	}
	for _, bad := range []string{"ca", "hostname"} {
		cfg := &tls.Config{RootCAs: pool}
		if bad == "ca" {
			cfg.RootCAs = x509.NewCertPool()
		} else {
			cfg.ServerName = "wrong.invalid"
		}
		dial, err := tlsprofile.DialTransport("native", shaping.Profile, nil, cfg)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		conn, err := dial(ctx, "tcp", s.Listener.Addr().String())
		cancel()
		if err == nil {
			conn.Close()
			t.Fatal("accepted", bad)
		}
	}
}

func TestShapingNeverDowngrades(t *testing.T) {
	var requests atomic.Int32
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1) }))
	defer s.Close()
	pool := x509.NewCertPool()
	pool.AddCert(s.Certificate())
	tr := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}, TLSHandshakeTimeout: time.Second}
	if err := tlsprofile.ConfigureTransport(tr, "native", shaping.Profile); err != nil {
		t.Fatal(err)
	}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: time.Second}
	resp, err := client.Get(s.URL)
	if err == nil {
		resp.Body.Close()
		t.Fatal("unshaped server accepted")
	}
	if requests.Load() != 0 {
		t.Fatal("plain HTTP retry leaked to application")
	}
}

func TestRejectUnknownShapingPolicy(t *testing.T) {
	if shaping.Check("future-v9") == nil {
		t.Fatal("unknown version accepted")
	}
	for _, cfg := range []*tls.Config{nil, {InsecureSkipVerify: true}} {
		if _, err := tlsprofile.DialTransport("native", shaping.Profile, nil, cfg); err == nil {
			t.Fatal("unverified shaping accepted")
		}
	}
}

func TestUnknownWireVersionAndSilentHandshakeIsolation(t *testing.T) {
	seed := httptest.NewTLSServer(nil)
	defer seed.Close()
	pool := x509.NewCertPool()
	pool.AddCert(seed.Certificate())
	var requests atomic.Int32
	s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.Write([]byte("ok")) }))
	s.Config.Handler = shaping.ConfigureServer(s.Config, s.Config.Handler)
	s.Listener = shaping.TLSListener(s.Listener, seed.TLS)
	s.Start()
	defer s.Close()
	// The accept loop must not perform a blocking handshake itself.
	silent, err := net.Dial("tcp", s.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer silent.Close()
	cfg := &tls.Config{RootCAs: pool, NextProtos: []string{"http/1.1"}}
	tr := &http.Transport{TLSClientConfig: cfg}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: time.Second}
	r, err := client.Get(strings.Replace(s.URL, "http://", "https://", 1))
	if err != nil {
		t.Fatal("silent peer blocked listener", err)
	}
	io.Copy(io.Discard, r.Body)
	r.Body.Close()
	for _, version := range [][]byte{{0, 'T', 2, 0}, {0, 'X', 1, 0}, {0, 'T', 1, 1}} {
		conn, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", s.Listener.Addr().String(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		conn.SetDeadline(time.Now().Add(time.Second))
		if _, err := conn.Write(version); err != nil {
			t.Fatal(err)
		}
		_, err = conn.Read(make([]byte, 32))
		conn.Close()
		if err == nil {
			t.Fatal("unknown shaping version accepted")
		}
		if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
			t.Fatal("unknown version was not closed")
		}
	}
	if requests.Load() != 1 {
		t.Fatal("unknown version reached application")
	}
}
