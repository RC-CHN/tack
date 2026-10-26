//go:build go1.25

// Package controlapp composes the hosted manager and embedded Chisel server.
package controlapp

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"time"

	chserver "github.com/jpillora/chisel/server"
	"github.com/jpillora/chisel/share/ccrypto"
	"github.com/jpillora/chisel/share/cio"
	"tack.local/management/control/command"
	p "tack.local/management/protocol"
)

func Support() command.TunnelSupport {
	return command.TunnelSupport{New: NewTunnel, GenerateKey: GenerateKey}
}

type tunnel struct {
	server     *chserver.Server
	host, port string
}

func (t *tunnel) Start(ctx context.Context) error { return t.server.StartContext(ctx, t.host, t.port) }
func (t *tunnel) Wait() error                     { return t.server.Wait() }
func (t *tunnel) Close() error                    { return t.server.Close() }
func (t *tunnel) Fingerprint() string             { return t.server.GetFingerprint() }

// NewTunnel validates all file-backed configuration before opening listeners.
// Deliberately do not inherit AUTH/CHISEL_KEY or share management credentials.
func NewTunnel(config p.TunnelServerConfig, logging bool) (command.Tunnel, error) {
	host, port, err := net.SplitHostPort(config.Listen)
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}
	if config.KeyFile == "" || config.AuthFile == "" {
		return nil, errors.New("key_file and auth_file are required")
	}
	if (config.TLSCert == "") != (config.TLSKey == "") {
		return nil, errors.New("provide both tunnel tls_cert and tls_key")
	}
	if config.TLSCert == "" {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return nil, errors.New("plaintext tunnel listener must bind a literal loopback address; configure TLS for remote access")
		}
	} else if _, err := tls.LoadX509KeyPair(config.TLSCert, config.TLSKey); err != nil {
		return nil, err
	}
	seconds := 25
	if config.KeepAliveSeconds != nil {
		seconds = *config.KeepAliveSeconds
	}
	if seconds < 0 || seconds > 86400 {
		return nil, errors.New("keep_alive_seconds must be 0..86400")
	}
	// Require an actual file, not the standalone tool's inline-key convenience.
	// This keeps deployment keys out of configuration values and process argv.
	if _, err := os.ReadFile(config.KeyFile); err != nil {
		return nil, fmt.Errorf("key_file: %w", err)
	}
	logger := cio.NewLogger("tunnel-server")
	logger.Info = logging
	if logging {
		logger.SetOutput(log.Writer())
	} else {
		logger.SetOutput(io.Discard)
	}
	server, err := chserver.NewServer(&chserver.Config{Logger: logger, RequireAuth: true,
		KeyFile: config.KeyFile, AuthFile: config.AuthFile, Reverse: config.Reverse, Socks5: config.Socks5,
		KeepAlive: time.Duration(seconds) * time.Second, TLS: chserver.TLSConfig{Key: config.TLSKey, Cert: config.TLSCert}})
	if err != nil {
		return nil, err
	}
	return &tunnel{server: server, host: host, port: port}, nil
}

// GenerateKey creates a private key exclusively. It neither overwrites existing
// keys nor writes private key bytes to stdout or a log.
func GenerateKey(path string) error {
	if path == "" || path == "-" {
		return errors.New("tunnel key generation requires a new file path")
	}
	key, err := ccrypto.GenerateKey("")
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	success := false
	defer func() {
		file.Close()
		if !success {
			os.Remove(path)
		}
	}()
	if _, err := file.Write(key); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	success = true
	return nil
}
