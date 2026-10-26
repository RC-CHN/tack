package integration_test

import (
	"io"
	"net/http"
	"strings"
	"testing"

	p "tack.local/management/protocol"
	"tack.local/management/protocol/tlsprofile"
)

func TestShapingPreservesAuthenticationAndAdminIsolation(t *testing.T) {
	t.Setenv("TACK_TRAFFIC_SHAPING", "bounded-v1")
	f := newNetworkFixture(t, nil)
	c, _, err := p.HTTPClient(f.nodeConfig.Server, f.nodeConfig.CAFile, false)
	if err != nil {
		t.Fatal(err)
	}
	if err = tlsprofile.ConfigureTransport(c.Transport.(*http.Transport), "native", "bounded-v1"); err != nil {
		t.Fatal(err)
	}
	defer c.CloseIdleConnections()
	for _, entry := range []struct {
		path, token string
		status      int
	}{
		{p.NodeRetentionPath, "", 401},
		{p.NodeRetentionPath, strings.Repeat("z", 40), 401},
		{p.NodeRetentionPath, f.config.AdminToken, 401},
		{"/v1/tasks", f.config.AdminToken, 404},
		{"/v1/tasks", f.nodeConfig.Token, 404},
	} {
		r, err := http.NewRequest("POST", f.nodeConfig.Server+entry.path, strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Authorization", "Bearer "+entry.token)
		resp, err := c.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != entry.status {
			t.Fatal(entry.path, resp.StatusCode, entry.status)
		}
	}
}
