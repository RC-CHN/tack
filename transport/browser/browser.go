// Package browser provides opt-in, pinned and personal uTLS ClientHello profiles. It does
// not emulate the browser's full network stack or its application behavior.
package browser

import (
	"context"
	"crypto/tls"
	"errors"
	"net"

	utls "github.com/refraction-networking/utls"
	"tack.local/management/protocol/tlsprofile"
)

const Profile = "chrome-133-h1"
const FullProfile = "chrome-133"
const PersonalizedProfile = "personalized-v1"

func init() {
	tlsprofile.Register(Profile, handshake)
	tlsprofile.Register(FullProfile, func(ctx context.Context, raw net.Conn, cfg *tls.Config) (net.Conn, error) {
		return handshakeProfile(ctx, raw, cfg, false)
	})
	tlsprofile.RegisterIdentity(PersonalizedProfile, func(ctx context.Context, raw net.Conn, cfg *tls.Config, seed [32]byte) (net.Conn, error) {
		spec, err := personalizedSpec(seed)
		if err != nil {
			return nil, err
		}
		return handshakeSpec(ctx, raw, cfg, spec)
	})
}

func handshake(ctx context.Context, raw net.Conn, cfg *tls.Config) (net.Conn, error) {
	return handshakeProfile(ctx, raw, cfg, true)
}

func handshakeProfile(ctx context.Context, raw net.Conn, cfg *tls.Config, h1only bool) (net.Conn, error) {
	spec, err := utls.UTLSIdToSpec(utls.HelloChrome_133)
	if err != nil {
		return nil, err
	}
	if h1only {
		filtered := spec.Extensions[:0]
		for _, extension := range spec.Extensions {
			switch e := extension.(type) {
			case *utls.ALPNExtension:
				e.AlpnProtocols = []string{"http/1.1"}
			case *utls.ApplicationSettingsExtension, *utls.ApplicationSettingsExtensionNew:
				continue
			}
			filtered = append(filtered, extension)
		}
		spec.Extensions = filtered
	}
	return handshakeSpec(ctx, raw, cfg, spec)
}

func handshakeSpec(ctx context.Context, raw net.Conn, cfg *tls.Config, spec utls.ClientHelloSpec) (net.Conn, error) {
	// Do not silently drop callers' custom verification or client-auth policy.
	if cfg.InsecureSkipVerify || cfg.VerifyPeerCertificate != nil || cfg.VerifyConnection != nil || cfg.GetClientCertificate != nil || len(cfg.Certificates) != 0 {
		return nil, errors.New("browser TLS profile requires normal server verification and does not yet support client certificates or verification callbacks")
	}
	if cfg.MaxVersion != 0 || (cfg.MinVersion != 0 && cfg.MinVersion != tls.VersionTLS12) || len(cfg.CipherSuites) != 0 || len(cfg.CurvePreferences) != 0 || cfg.Renegotiation != tls.RenegotiateNever || cfg.KeyLogWriter != nil {
		return nil, errors.New("browser TLS profile does not support custom TLS version, cipher, curve, renegotiation or key logging policy")
	}
	ucfg := &utls.Config{ServerName: cfg.ServerName, RootCAs: cfg.RootCAs, MinVersion: utls.VersionTLS12}
	conn := utls.UClient(raw, ucfg, utls.HelloCustom)
	if err := conn.ApplyPreset(&spec); err != nil {
		return nil, err
	}
	if err := conn.HandshakeContext(ctx); err != nil {
		return nil, err
	}
	if alpn := conn.ConnectionState().NegotiatedProtocol; alpn != "" && alpn != "http/1.1" {
		return nil, errors.New("unexpected ALPN for browser HTTP/1.1 profile")
	}
	return conn, nil
}
