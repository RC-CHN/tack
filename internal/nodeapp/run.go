// Package nodeapp composes the management worker and embedded Chisel client.
package nodeapp

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	chclient "github.com/jpillora/chisel/client"
	"github.com/jpillora/chisel/share/cio"
	"github.com/jpillora/chisel/share/settings"
	"tack.local/management/node"
	p "tack.local/management/protocol"
)

// Run owns both components for the lifetime of the single foreground process.
// Installation, startup registration and directory selection remain external.
func Run(ctx context.Context, config p.NodeConfig, logging bool) error {
	var service func(context.Context) error
	if config.Tunnel != nil {
		client, err := newTunnel(*config.Tunnel, config.AllowInsecureLoopback, logging)
		if err != nil {
			return fmt.Errorf("tunnel: %w", err)
		}
		service = func(ctx context.Context) error {
			if err := client.Start(ctx); err != nil {
				return err
			}
			defer client.Close()
			return client.Wait()
		}
	}
	config.Tunnel = nil // consumed here; the management-only worker rejects it
	a, err := node.New(config)
	if err != nil {
		return err
	}
	if !logging {
		a.Logf = nil
	}
	return a.RunWithService(ctx, service)
}

func newTunnel(config p.TunnelConfig, allowLoopback, logging bool) (*chclient.Client, error) {
	// Reuse management's verified-HTTPS / explicit local-development boundary.
	httpClient, _, err := p.HTTPClient(config.Server, config.CAFile, allowLoopback)
	if err != nil {
		return nil, err
	}
	httpClient.CloseIdleConnections()
	user, password, ok := strings.Cut(config.Auth, ":")
	if !ok || user == "" || password == "" {
		return nil, errors.New("auth must contain a node-specific username:password")
	}
	fingerprint, err := base64.StdEncoding.DecodeString(config.Fingerprint)
	if err != nil || len(fingerprint) != 32 {
		return nil, errors.New("fingerprint must be the server's complete SHA-256 fingerprint")
	}
	if len(config.Remotes) == 0 || len(config.Remotes) > 64 {
		return nil, errors.New("remotes must contain 1..64 tunnel endpoints")
	}
	for _, remote := range config.Remotes {
		r, err := settings.DecodeRemote(remote)
		if err != nil {
			return nil, fmt.Errorf("invalid remote: %w", err)
		}
		if r.Stdio {
			return nil, errors.New("stdio remotes cannot be used by the foreground Node service")
		}
	}
	seconds := 25 // standalone Chisel's default; explicitly set 0 to disable
	if config.KeepAliveSeconds != nil {
		seconds = *config.KeepAliveSeconds
	}
	if seconds < 0 || seconds > 86400 {
		return nil, errors.New("keep_alive_seconds must be 0..86400")
	}
	logger := cio.NewLogger("tunnel")
	logger.Info = logging
	if logging {
		logger.SetOutput(log.Writer())
	} else {
		logger.SetOutput(io.Discard)
	}
	return chclient.NewClient(&chclient.Config{
		Logger: logger,
		Server: config.Server, Auth: config.Auth, Fingerprint: config.Fingerprint,
		Remotes: config.Remotes, KeepAlive: time.Duration(seconds) * time.Second,
		MaxRetryCount: -1,
		TLS:           chclient.TLSConfig{CA: config.CAFile},
	})
}
