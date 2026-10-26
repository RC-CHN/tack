// Package command owns the hosted controller's configuration and lifecycle.
package command

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"tack.local/management/control"
	p "tack.local/management/protocol"
	"tack.local/management/protocol/rotatelog"
	"tack.local/management/protocol/shaping"
)

// Tunnel is owned by Run. Close must interrupt active connections; Wait must
// finish all background work. The implementation lives outside this module.
type Tunnel interface {
	Start(context.Context) error
	Wait() error
	Close() error
	Fingerprint() string
}
type TunnelSupport struct {
	New         func(p.TunnelServerConfig, bool) (Tunnel, error)
	GenerateKey func(string) error
}
type Options struct {
	State, Listen, AdminListen, TLSCert, TLSKey, TrafficShaping string
	Logging                                                     bool
}

func Main() { MainWithTunnel(TunnelSupport{}) }
func MainWithTunnel(support TunnelSupport) {
	if err := run(support); err != nil {
		log.Fatal(err)
	}
}
func run(support TunnelSupport) error {
	version := flag.Bool("version", false, "print release version")
	configPath := flag.String("config", "", "controller JSON configuration")
	state := flag.String("state", "", "task state file (required for serving)")
	listen := flag.String("listen", "127.0.0.1:8443", "Node listener address")
	admin := flag.String("admin-listen", "127.0.0.1:8081", "administrator listener (literal loopback only)")
	cert := flag.String("tls-cert", "", "Node TLS certificate file")
	key := flag.String("tls-key", "", "Node TLS key file")
	shape := flag.String("traffic-shaping", "off", "Node TLS listener shaping: off or bounded-v1 (HTTP/1.1)")
	keygen := flag.String("tunnel-keygen", "", "create a new tunnel SSH private key file; never overwrite an existing file")
	fingerprint := flag.Bool("tunnel-fingerprint", false, "validate configured tunnel and print its SSH fingerprint without starting listeners")
	logs := rotatelog.Flags(flag.CommandLine)
	flag.Parse()
	if *version {
		fmt.Println(p.BuildVersion)
		return nil
	}
	if *keygen != "" {
		if *fingerprint {
			return errors.New("choose either -tunnel-keygen or -tunnel-fingerprint")
		}
		if support.GenerateKey == nil {
			return errors.New("tunnel key generation requires the unified tack-control binary")
		}
		return support.GenerateKey(*keygen)
	}
	closeLog, err := logs.Install()
	if err != nil {
		return err
	}
	defer closeLog()
	if *configPath == "" {
		return errors.New("-config is required")
	}
	var config p.ControlConfig
	if err := p.ReadJSON(*configPath, &config); err != nil {
		return err
	}
	if *fingerprint {
		if config.Tunnel == nil || support.New == nil {
			return errors.New("tunnel fingerprint requires a tunnel configuration and unified tack-control binary")
		}
		service, err := support.New(*config.Tunnel, false)
		if err != nil {
			return err
		}
		defer service.Close()
		fmt.Println(service.Fingerprint())
		return nil
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return Run(ctx, config, Options{State: *state, Listen: *listen, AdminListen: *admin, TLSCert: *cert,
		TLSKey: *key, TrafficShaping: *shape, Logging: logs.Enabled()}, support)
}

func loopback(address, label string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("%s must bind a literal loopback address", label)
	}
	return nil
}

// Run serves management and an optional embedded tunnel as one lifecycle. A
// listener failure stops every component; old configurations without tunnel
// retain their management-only behavior.
func Run(ctx context.Context, config p.ControlConfig, options Options, support TunnelSupport) error {
	if options.State == "" {
		return errors.New("-state is required")
	}
	if err := shaping.Check(options.TrafficShaping); err != nil {
		return err
	}
	if (options.TLSCert == "") != (options.TLSKey == "") {
		return errors.New("provide both TLS certificate and key")
	}
	if err := loopback(options.AdminListen, "administrator listener"); err != nil {
		return err
	}
	var tlsConfig *tls.Config
	if options.TLSCert == "" {
		if shaping.Enabled(options.TrafficShaping) {
			return errors.New("traffic shaping requires TLS certificate and key")
		}
		if err := loopback(options.Listen, "plaintext Node listener"); err != nil {
			return err
		}
	} else {
		pair, err := tls.LoadX509KeyPair(options.TLSCert, options.TLSKey)
		if err != nil {
			return err
		}
		tlsConfig = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{pair}}
	}
	if err := control.ValidateConfig(config); err != nil {
		return err
	}
	var service Tunnel
	if config.Tunnel != nil {
		if support.New == nil {
			return errors.New("tunnel configuration requires the unified tack-control binary")
		}
		var err error
		service, err = support.New(*config.Tunnel, options.Logging)
		if err != nil {
			return fmt.Errorf("tunnel: %w", err)
		}
		defer service.Close()
	}
	if err := os.MkdirAll(filepath.Dir(options.State), 0700); err != nil {
		return err
	}
	unlock, err := p.Lock(options.State + ".lock")
	if err != nil {
		return err
	}
	defer unlock()
	api, err := control.New(config, options.State)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	srv := &http.Server{BaseContext: func(net.Listener) context.Context { return ctx }, Handler: api.NodeHandler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 20 * time.Second, WriteTimeout: 20 * time.Second, IdleTimeout: 60 * time.Second, TLSConfig: tlsConfig}
	admin := &http.Server{BaseContext: func(net.Listener) context.Context { return ctx }, Handler: api.AdminHandler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 20 * time.Second, WriteTimeout: 20 * time.Second, IdleTimeout: 60 * time.Second}
	// Bind both management listeners before launching any request workers.
	nodeListener, err := net.Listen("tcp", options.Listen)
	if err != nil {
		return err
	}
	defer nodeListener.Close()
	adminListener, err := net.Listen("tcp", options.AdminListen)
	if err != nil {
		return err
	}
	defer adminListener.Close()
	if service != nil {
		if err := service.Start(ctx); err != nil {
			return fmt.Errorf("tunnel: %w", err)
		}
	}
	if tlsConfig != nil && shaping.Enabled(options.TrafficShaping) {
		nodeListener = shaping.TLSListener(nodeListener, tlsConfig)
		srv.Handler = shaping.ConfigureServer(srv, srv.Handler)
	}
	errorsCh := make(chan error, 3)
	var workers sync.WaitGroup
	serve := func(f func() error) { workers.Add(1); go func() { defer workers.Done(); errorsCh <- f() }() }
	serve(func() error {
		if tlsConfig != nil && !shaping.Enabled(options.TrafficShaping) {
			return srv.ServeTLS(nodeListener, "", "")
		}
		return srv.Serve(nodeListener)
	})
	serve(func() error { return admin.Serve(adminListener) })
	if service != nil {
		serve(service.Wait)
	}
	workers.Add(1)
	go func() {
		defer workers.Done()
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := api.Maintain(); err != nil {
					log.Printf("history maintenance: %v", err)
				}
			}
		}
	}()
	log.Printf("%s; Node endpoint %s; administrator endpoint %s", api, options.Listen, options.AdminListen)
	select {
	case err = <-errorsCh:
	case <-ctx.Done():
		err = nil
	}
	cancel()
	if service != nil {
		service.Close()
	}
	shutdown, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	if srv.Shutdown(shutdown) != nil {
		srv.Close()
	}
	if admin.Shutdown(shutdown) != nil {
		admin.Close()
	}
	workers.Wait()
	if errors.Is(err, http.ErrServerClosed) || errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}
