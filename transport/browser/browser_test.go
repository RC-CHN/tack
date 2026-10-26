package browser

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tack.local/management/protocol/tlsprofile"
)

type capture struct {
	net.Conn
	mu    sync.Mutex
	bytes []byte
}

func (c *capture) Write(p []byte) (int, error) {
	c.mu.Lock()
	if len(c.bytes)+len(p) <= 128*1024 {
		c.bytes = append(c.bytes, p...)
	}
	c.mu.Unlock()
	return c.Conn.Write(p)
}

func tlsServer(t testing.TB) (*httptest.Server, *tls.Config) {
	t.Helper()
	s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 1 {
			t.Errorf("unexpected protocol %s", r.Proto)
		}
		io.Copy(w, r.Body)
	}))
	// Deliberately an HTTP/1.1 server. Full Chrome advertises h2 as well; the
	// server must select h1 before this implementation sends an HTTP request.
	s.EnableHTTP2 = false
	s.StartTLS()
	t.Cleanup(s.Close)
	pool := x509.NewCertPool()
	pool.AddCert(s.Certificate())
	return s, &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
}

func TestBrowserHTTPSVerificationAndALPN(t *testing.T) {
	s, cfg := tlsServer(t)
	for _, profile := range []string{Profile, FullProfile, PersonalizedProfile} {
		for _, bad := range []string{"", "ca", "hostname"} {
			t.Run(profile+"/"+bad, func(t *testing.T) {
				config := cfg.Clone()
				if bad == "ca" {
					config.RootCAs = x509.NewCertPool()
				}
				if bad == "hostname" {
					config.ServerName = "wrong.invalid"
				}
				transport := &http.Transport{TLSClientConfig: config, TLSHandshakeTimeout: time.Second}
				var identity *tlsprofile.Identity
				if tlsprofile.NeedsIdentity(profile) {
					var err error
					identity, err = tlsprofile.LoadIdentity(filepath.Join(t.TempDir(), "identity.json"))
					if err != nil {
						t.Fatal(err)
					}
				}
				if err := tlsprofile.ConfigureHTTP(transport, profile, identity); err != nil {
					t.Fatal(err)
				}
				defer transport.CloseIdleConnections()
				client := &http.Client{Transport: transport, Timeout: 2 * time.Second}
				r, err := client.Post(s.URL, "text/plain", bytes.NewBufferString("verified echo"))
				if bad != "" {
					if err == nil {
						r.Body.Close()
						t.Fatal("accepted invalid certificate")
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				defer r.Body.Close()
				data, err := io.ReadAll(r.Body)
				if err != nil || string(data) != "verified echo" {
					t.Fatalf("echo %q: %v", data, err)
				}
			})
		}
	}
}

func TestBrowserCancellationClosesSilentPeer(t *testing.T) {
	for _, profile := range []string{Profile, PersonalizedProfile} {
		t.Run(profile, func(t *testing.T) {
			left, right := net.Pipe()
			defer right.Close()
			go io.Copy(io.Discard, right)
			var identity *tlsprofile.Identity
			if tlsprofile.NeedsIdentity(profile) {
				var err error
				identity, err = tlsprofile.LoadIdentity(filepath.Join(t.TempDir(), "identity.json"))
				if err != nil {
					t.Fatal(err)
				}
			}
			dial, err := tlsprofile.DialTLS(profile, func(context.Context, string, string) (net.Conn, error) { return left, nil }, &tls.Config{}, identity)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			start := time.Now()
			if c, err := dial(ctx, "tcp", "example.invalid:443"); err == nil {
				c.Close()
				t.Fatal("silent handshake succeeded")
			}
			if time.Since(start) > time.Second {
				t.Fatal("cancellation did not bound handshake")
			}
			if _, err := left.Write([]byte{1}); err == nil {
				t.Fatal("raw connection leaked")
			}
		})
	}
}

func TestFullProfileRefusesHTTP2WithoutRetry(t *testing.T) {
	for _, profile := range []string{FullProfile, PersonalizedProfile} {
		t.Run(profile, func(t *testing.T) {
			var requests atomic.Int32
			s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1) }))
			s.EnableHTTP2 = true
			s.StartTLS()
			defer s.Close()
			pool := x509.NewCertPool()
			pool.AddCert(s.Certificate())
			tr := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}, TLSHandshakeTimeout: time.Second}
			var identity *tlsprofile.Identity
			if tlsprofile.NeedsIdentity(profile) {
				var err error
				identity, err = tlsprofile.LoadIdentity(filepath.Join(t.TempDir(), "identity.json"))
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := tlsprofile.ConfigureHTTP(tr, profile, identity); err != nil {
				t.Fatal(err)
			}
			defer tr.CloseIdleConnections()
			c := &http.Client{Transport: tr, Timeout: 2 * time.Second}
			r, err := c.Get(s.URL)
			if err == nil {
				r.Body.Close()
				t.Fatal("sent HTTP/1.1 over negotiated h2")
			}
			if requests.Load() != 0 {
				t.Fatal("silently retried with a different fingerprint")
			}
		})
	}
}

func TestBrowserRefusesUnsupportedPolicy(t *testing.T) {
	for _, cfg := range []*tls.Config{
		{InsecureSkipVerify: true},
		{Certificates: []tls.Certificate{{}}},
		{MinVersion: tls.VersionTLS13},
		{MaxVersion: tls.VersionTLS12},
		{CipherSuites: []uint16{tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256}},
		{CurvePreferences: []tls.CurveID{tls.X25519}},
		{VerifyConnection: func(tls.ConnectionState) error { return nil }},
	} {
		left, right := net.Pipe()
		_, err := handshakeProfile(context.Background(), left, cfg, false)
		left.Close()
		right.Close()
		if err == nil {
			t.Fatal("ignored unsupported policy")
		}
	}
}

type helloSummary struct {
	Bytes          int      `json:"client_hello_record_bytes"`
	Ciphers        []uint16 `json:"cipher_suites"`
	Extensions     []uint16 `json:"extensions"`
	ALPN           []string `json:"alpn"`
	Groups         []uint16 `json:"supported_groups,omitempty"`
	Signatures     []uint16 `json:"signature_algorithms,omitempty"`
	Versions       []uint16 `json:"supported_versions,omitempty"`
	KeyShareGroups []uint16 `json:"key_share_groups,omitempty"`
}

// Parse the captured wire bytes, independently of the configuration template.
func parseHello(t testing.TB, wire []byte) helloSummary {
	t.Helper()
	if len(wire) < 44 || wire[0] != 22 || wire[5] != 1 {
		t.Fatal("missing ClientHello record")
	}
	n := 5 + int(binary.BigEndian.Uint16(wire[3:5]))
	if n > len(wire) {
		t.Fatal("truncated record")
	}
	p := wire[43:n]
	if len(p) < 1 || int(p[0])+1 > len(p) {
		t.Fatal("invalid session id")
	}
	p = p[1+int(p[0]):]
	length := int(binary.BigEndian.Uint16(p[:2]))
	p = p[2:]
	out := helloSummary{Bytes: n}
	for i := 0; i < length; i += 2 {
		out.Ciphers = append(out.Ciphers, binary.BigEndian.Uint16(p[i:i+2]))
	}
	p = p[length:]
	p = p[1+int(p[0]):]
	p = p[2:]
	for len(p) > 0 {
		if len(p) < 4 {
			t.Fatal("short extension")
		}
		id, size := binary.BigEndian.Uint16(p[:2]), int(binary.BigEndian.Uint16(p[2:4]))
		p = p[4:]
		if size > len(p) {
			t.Fatal("truncated extension")
		}
		out.Extensions = append(out.Extensions, id)
		switch id {
		case 10, 13, 43:
			start := 2
			if id == 43 {
				start = 1
			}
			var ids []uint16
			for j := start; j+1 < size; j += 2 {
				ids = append(ids, binary.BigEndian.Uint16(p[j:j+2]))
			}
			switch id {
			case 10:
				out.Groups = ids
			case 13:
				out.Signatures = ids
			case 43:
				out.Versions = ids
			}
		case 51:
			for j := 2; j+4 <= size; {
				out.KeyShareGroups = append(out.KeyShareGroups, binary.BigEndian.Uint16(p[j:j+2]))
				j += 4 + int(binary.BigEndian.Uint16(p[j+2:j+4]))
			}
		}
		if id == 16 && size >= 2 {
			alpn := p[2:size]
			for len(alpn) > 0 {
				length := int(alpn[0])
				alpn = alpn[1:]
				if length > len(alpn) {
					t.Fatal("invalid ALPN")
				}
				out.ALPN = append(out.ALPN, string(alpn[:length]))
				alpn = alpn[length:]
			}
		}
		p = p[size:]
	}
	return out
}

func TestTLSWireExperiment(t *testing.T) {
	s, cfg := tlsServer(t)
	type result struct {
		Profile         string       `json:"profile"`
		HandshakeMicros []int64      `json:"handshake_and_echo_us"`
		UpstreamBytes   []int        `json:"upstream_tls_bytes"`
		Hello           helloSummary `json:"hello"`
	}
	results := []result{{Profile: "native"}, {Profile: Profile}, {Profile: FullProfile}, {Profile: PersonalizedProfile}}
	identity, err := tlsprofile.LoadIdentity(filepath.Join(t.TempDir(), "identity.json"))
	if err != nil {
		t.Fatal(err)
	}
	for sample := 0; sample < 20; sample++ {
		// Alternate order to reduce warm-up bias. Each sample uses fresh TLS.
		for step := 0; step < len(results); step++ {
			i := (sample + step) % len(results)
			var wire *capture
			transport := &http.Transport{TLSClientConfig: cfg.Clone(), TLSHandshakeTimeout: time.Second, DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
				if err != nil {
					return nil, err
				}
				wire = &capture{Conn: c}
				return wire, nil
			}}
			var clientIdentity *tlsprofile.Identity
			if results[i].Profile == PersonalizedProfile {
				clientIdentity = identity
			}
			if err := tlsprofile.ConfigureHTTP(transport, results[i].Profile, clientIdentity); err != nil {
				t.Fatal(err)
			}
			client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
			start := time.Now()
			r, err := client.Post(s.URL, "application/octet-stream", bytes.NewReader(bytes.Repeat([]byte{42}, 1024)))
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(r.Body)
			r.Body.Close()
			if err != nil || len(body) != 1024 {
				t.Fatal("incorrect echo", err)
			}
			elapsed := time.Since(start).Microseconds()
			wire.mu.Lock()
			results[i].HandshakeMicros = append(results[i].HandshakeMicros, elapsed)
			results[i].UpstreamBytes = append(results[i].UpstreamBytes, len(wire.bytes))
			if sample == 0 {
				results[i].Hello = parseHello(t, wire.bytes)
			}
			wire.mu.Unlock()
			transport.CloseIdleConnections()
		}
	}
	if bytes.Equal(mustJSON(results[0].Hello.Ciphers), mustJSON(results[1].Hello.Ciphers)) {
		t.Fatal("fingerprint did not change")
	}
	if !bytes.Equal(mustJSON(results[2].Hello.ALPN), mustJSON([]string{"h2", "http/1.1"})) {
		t.Fatal("full profile changed ALPN", results[2].Hello.ALPN)
	}
	data, _ := json.MarshalIndent(results, "", "  ")
	t.Log(string(data))
	if path := os.Getenv("TACK_TLS_REPORT"); path != "" {
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }
