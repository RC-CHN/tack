package tlsprofile

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/url"
	"testing"
)

func TestUnavailableDoesNotFallBack(t *testing.T) {
	if Check("chrome-133-h1") == nil {
		t.Fatal("provider unexpectedly linked into Legacy module")
	}
	transport := &http.Transport{}
	if err := ConfigureHTTP(transport, "missing"); err == nil || transport.DialTLSContext != nil {
		t.Fatal("unknown profile accepted")
	}
	if err := ConfigureHTTP(transport, "native"); err != nil || transport.DialTLSContext != nil {
		t.Fatal("native transport changed")
	}
}

func TestNoSilentProxyOrVerificationBypass(t *testing.T) {
	// The handshake is never reached: this test covers transport policy only.
	Register("policy-test", func(_ context.Context, c net.Conn, _ *tls.Config) (net.Conn, error) { return c, nil })
	defer func() { providers.Lock(); delete(providers.items, "policy-test"); providers.Unlock() }()
	if _, err := DialTLS("policy-test", nil, &tls.Config{InsecureSkipVerify: true}); err == nil {
		t.Fatal("verification bypass accepted")
	}
	proxy, _ := url.Parse("http://127.0.0.1:8080")
	tr := &http.Transport{TLSClientConfig: &tls.Config{}, Proxy: http.ProxyURL(proxy)}
	if err := ConfigureHTTP(tr, "policy-test"); err != nil {
		t.Fatal(err)
	}
	r, _ := http.NewRequest("GET", "https://example.invalid", nil)
	if _, err := tr.Proxy(r); err == nil {
		t.Fatal("proxy silently used native TLS")
	}
}
