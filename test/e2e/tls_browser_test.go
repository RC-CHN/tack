//go:build browser

package e2e_test

import (
	chclient "github.com/jpillora/chisel/client"
	chserver "github.com/jpillora/chisel/server"
	"path/filepath"
	"testing"
)

func TestBrowserTLSForwardAndReverse(t *testing.T) {
	for _, profile := range []string{"chrome-133", "personalized-v1"} {
		for _, reverse := range []bool{false, true} {
			name := "forward"
			if reverse {
				name = "reverse"
			}
			t.Run(profile+"/"+name, func(t *testing.T) {
				certs, err := newTestTLSConfig()
				if err != nil {
					t.Fatal(err)
				}
				defer certs.Close()
				certs.serverTLS.CA = ""
				certs.clientTLS.Cert = ""
				certs.clientTLS.Key = ""
				certs.clientTLS.Profile = profile
				if profile == "personalized-v1" {
					certs.clientTLS.IdentityFile = filepath.Join(t.TempDir(), "tls-identity.json")
				}
				certs.clientTLS.TrafficShaping = "bounded-v1"
				certs.serverTLS.TrafficShaping = "bounded-v1"
				port := availablePort()
				remote := port + ":$FILEPORT"
				if reverse {
					remote = "R:" + remote
				}
				teardown := simpleSetup(t, &chserver.Config{TLS: *certs.serverTLS, Reverse: reverse}, &chclient.Config{TLS: *certs.clientTLS, Remotes: []string{remote}})
				defer teardown()
				got, err := post("http://localhost:"+port, "browser-transport")
				if err != nil || got != "browser-transport!" {
					t.Fatal(got, err)
				}
			})
		}
	}
}

func TestBrowserTLSRecoversAcrossImpairedTransport(t *testing.T) {
	for _, profile := range []string{"chrome-133", "personalized-v1"} {
		t.Run(profile, func(t *testing.T) {
			certs, err := newTestTLSConfig()
			if err != nil {
				t.Fatal(err)
			}
			defer certs.Close()
			certs.serverTLS.CA = ""
			certs.clientTLS.Cert = ""
			certs.clientTLS.Key = ""
			certs.clientTLS.Profile = profile
			if profile == "personalized-v1" {
				certs.clientTLS.IdentityFile = filepath.Join(t.TempDir(), "tls-identity.json")
			}
			certs.clientTLS.TrafficShaping = "bounded-v1"
			certs.serverTLS.TrafficShaping = "bounded-v1"
			tunnelRecoversAcrossImpairedTransport(t, certs)
		})
	}
}
