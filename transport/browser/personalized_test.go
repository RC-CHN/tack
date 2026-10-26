package browser

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	utls "github.com/refraction-networking/utls"
	"tack.local/management/protocol/tlsprofile"
)

func fixtureIdentity(t testing.TB, path string, seed [32]byte) *tlsprofile.Identity {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"version": 1, "seed": hex.EncodeToString(seed[:])})
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	id, err := tlsprofile.LoadIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func normalizeGrease(ids []uint16) {
	for i, id := range ids {
		if id&0x0f0f == 0x0a0a && byte(id>>8) == byte(id) {
			ids[i] = 0x0a0a
		}
	}
}

// The fingerprint excludes ClientRandom, session IDs, key material and GREASE
// values. Those must remain fresh even when template structure is persistent.
func personalWire(t testing.TB, s *httptest.Server, cfg *tls.Config, id *tlsprofile.Identity) ([]byte, helloSummary) {
	t.Helper()
	var wire *capture
	tr := &http.Transport{TLSClientConfig: cfg.Clone(), TLSHandshakeTimeout: time.Second, DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		wire = &capture{Conn: c}
		return wire, nil
	}}
	if err := tlsprofile.ConfigureHTTP(tr, PersonalizedProfile, id); err != nil {
		t.Fatal(err)
	}
	defer tr.CloseIdleConnections()
	c := &http.Client{Transport: tr, Timeout: 3 * time.Second}
	r, err := c.Post(s.URL, "text/plain", bytes.NewBufferString("personalized echo"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(r.Body)
	r.Body.Close()
	if err != nil || string(b) != "personalized echo" {
		t.Fatal("incorrect echo", err)
	}
	wire.mu.Lock()
	defer wire.mu.Unlock()
	data := append([]byte(nil), wire.bytes...)
	hello := parseHello(t, data)
	hello.Bytes = 0 // GREASE ECH payload length is intentionally not persistent.
	normalizeGrease(hello.Ciphers)
	normalizeGrease(hello.Extensions)
	normalizeGrease(hello.Groups)
	normalizeGrease(hello.Versions)
	normalizeGrease(hello.KeyShareGroups)
	return data, hello
}

func TestPersonalizedStableAcrossReloadAndDistinctClients(t *testing.T) {
	s, cfg := tlsServer(t)
	dir := t.TempDir()
	fingerprints := make(map[string]bool)
	for i := 0; i < 32; i++ {
		seed := sha256.Sum256([]byte(fmt.Sprintf("fixture-client-%d", i)))
		seed[0] = byte(i) // Exercise all combinations of optional capabilities.
		path := filepath.Join(dir, fmt.Sprintf("client-%d.json", i))
		id := fixtureIdentity(t, path, seed)
		first, hello := personalWire(t, s, cfg, id)
		reloaded, err := tlsprofile.LoadIdentity(path)
		if err != nil {
			t.Fatal(err)
		}
		second, again := personalWire(t, s, cfg, reloaded)
		if !reflect.DeepEqual(hello, again) {
			t.Fatalf("identity %d changed template", i)
		}
		if bytes.Equal(first[11:43], second[11:43]) {
			t.Fatal("reused TLS ClientRandom")
		}
		keyShares := func(wire []byte) []byte {
			n := 5 + int(binary.BigEndian.Uint16(wire[3:5]))
			p := wire[43:n]
			p = p[1+int(p[0]):]
			p = p[2+int(binary.BigEndian.Uint16(p[:2])):]
			p = p[1+int(p[0]):]
			p = p[2:]
			for len(p) >= 4 {
				id, size := binary.BigEndian.Uint16(p[:2]), int(binary.BigEndian.Uint16(p[2:4]))
				p = p[4:]
				if id == 51 {
					shares := p[2:size]
					for len(shares) >= 4 {
						group, n := binary.BigEndian.Uint16(shares[:2]), int(binary.BigEndian.Uint16(shares[2:4]))
						shares = shares[4:]
						if group == uint16(utls.X25519) {
							return shares[:n]
						}
						shares = shares[n:]
					}
					return nil
				}
				p = p[size:]
			}
			return nil
		}
		if len(keyShares(first)) == 0 || bytes.Equal(keyShares(first), keyShares(second)) {
			t.Fatal("missing or reused key shares")
		}
		fingerprint := string(mustJSON(hello))
		if fingerprints[fingerprint] {
			t.Fatal("distinct fixtures generated identical fingerprints")
		}
		fingerprints[fingerprint] = true
	}
	t.Log("32/32 distinct normalized templates; 64 verified TLS handshakes; all reloads stable, ClientRandom and key shares fresh")
}

func TestPersonalizedTLS12AndTemplateVersion(t *testing.T) {
	s, cfg := tlsServer(t)
	s.TLS.MaxVersion = tls.VersionTLS12 // No connection has been made yet.
	var fixtureDigests []string
	for i := 0; i < 32; i++ {
		var seed [32]byte
		seed[0] = byte(i)
		id := fixtureIdentity(t, filepath.Join(t.TempDir(), "identity.json"), seed)
		_, hello := personalWire(t, s, cfg, id)
		if !reflect.DeepEqual(hello.ALPN, []string{"h2", "http/1.1"}) {
			t.Fatal("changed ALPN")
		}
		spec, err := personalizedSpec(seed)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range spec.Extensions {
			if v, ok := e.(*utls.SupportedVersionsExtension); ok {
				for _, version := range v.Versions {
					if version != utls.GREASE_PLACEHOLDER && version < tls.VersionTLS12 {
						t.Fatal("advertised obsolete TLS")
					}
				}
			}
		}
		sum := sha256.Sum256(mustJSON(hello))
		fixtureDigests = append(fixtureDigests, hex.EncodeToString(sum[:]))
	}
	golden := sha256.Sum256(mustJSON(fixtureDigests))
	const expected = "358f6b3655b0a49ccef0525ca5c4cfe10dcb3eef9276ead97ecb8433c8e696f0"
	if got := hex.EncodeToString(golden[:]); got != expected {
		t.Fatalf("personalized-v1 template recipe changed: %s", got)
	}
}

func TestPersonalizedRequiresIdentity(t *testing.T) {
	if _, err := tlsprofile.DialTLS(PersonalizedProfile, nil, &tls.Config{}); err == nil {
		t.Fatal("missing identity silently randomized")
	}
}

func BenchmarkPersonalizedSpec(b *testing.B) {
	seed := sha256.Sum256([]byte("benchmark identity"))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := personalizedSpec(seed); err != nil {
			b.Fatal(err)
		}
	}
}
