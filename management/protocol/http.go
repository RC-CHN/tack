package protocol

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

func HTTPClient(server, caFile string, allowLoopback bool) (*http.Client, string, error) {
	u, err := url.Parse(server)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, "", errors.New("server must be an absolute HTTPS origin without credentials or a path")
	}
	if u.Scheme != "https" {
		ip := net.ParseIP(u.Hostname())
		if u.Scheme != "http" || !allowLoopback || ip == nil || !ip.IsLoopback() {
			return nil, "", errors.New("HTTPS required (HTTP only with explicit literal-loopback development option)")
		}
	}
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	if caFile != "" {
		b, err := os.ReadFile(caFile)
		if err != nil {
			return nil, "", err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(b) {
			return nil, "", errors.New("CA file contains no certificates")
		}
		t.TLSClientConfig.RootCAs = pool
	}
	return &http.Client{Transport: t, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirects are not allowed") }}, strings.TrimSuffix(u.String(), "/"), nil
}
