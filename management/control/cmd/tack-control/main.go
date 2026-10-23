package main

import (
	"context"
	"crypto/tls"
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
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}
func run() error {
	version := flag.Bool("version", false, "print release version")
	configPath := flag.String("config", "", "controller JSON configuration")
	statePath := flag.String("state", "", "task state file (required)")
	listen := flag.String("listen", "127.0.0.1:8443", "Node listener address")
	adminListen := flag.String("admin-listen", "127.0.0.1:8081", "administrator listener (literal loopback only)")
	cert := flag.String("tls-cert", "", "TLS certificate file")
	key := flag.String("tls-key", "", "TLS key file")
	logs := rotatelog.Flags(flag.CommandLine)
	flag.Parse()
	if *version {
		fmt.Println(p.BuildVersion)
		return nil
	}
	closeLog, logErr := logs.Install()
	if logErr != nil {
		return logErr
	}
	defer closeLog()
	if *configPath == "" || *statePath == "" {
		return fmt.Errorf("-config and -state are required")
	}
	if (*cert == "") != (*key == "") {
		return fmt.Errorf("provide both TLS certificate and key")
	}
	if *cert == "" {
		host, _, err := net.SplitHostPort(*listen)
		if err != nil {
			return err
		}
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return fmt.Errorf("plaintext listener must bind a literal loopback address; configure TLS for remote access")
		}
	}
	adminHost, _, err := net.SplitHostPort(*adminListen)
	if err != nil {
		return err
	}
	adminIP := net.ParseIP(adminHost)
	if adminIP == nil || !adminIP.IsLoopback() {
		return fmt.Errorf("administrator listener must bind a literal loopback address")
	}
	var config p.ControlConfig
	if err := p.ReadJSON(*configPath, &config); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(*statePath), 0700); err != nil {
		return err
	}
	unlock, err := p.Lock(*statePath + ".lock")
	if err != nil {
		return err
	}
	defer unlock()
	api, err := control.New(config, *statePath)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var maintenance sync.WaitGroup
	maintenance.Add(1)
	go func() {
		defer maintenance.Done()
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
	defer func() { stop(); maintenance.Wait() }()
	srv := &http.Server{BaseContext: func(net.Listener) context.Context { return ctx }, Addr: *listen, Handler: api.NodeHandler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 20 * time.Second, WriteTimeout: 20 * time.Second, IdleTimeout: 60 * time.Second, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12}}
	admin := &http.Server{BaseContext: func(net.Listener) context.Context { return ctx }, Addr: *adminListen, Handler: api.AdminHandler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 20 * time.Second, WriteTimeout: 20 * time.Second, IdleTimeout: 60 * time.Second}
	errorsCh := make(chan error, 2)
	go func() {
		if *cert != "" {
			errorsCh <- srv.ListenAndServeTLS(*cert, *key)
		} else {
			errorsCh <- srv.ListenAndServe()
		}
	}()
	go func() { errorsCh <- admin.ListenAndServe() }()
	log.Printf("%s; Node endpoint %s; administrator endpoint %s", api, *listen, *adminListen)
	select {
	case err = <-errorsCh:
	case <-ctx.Done():
		err = nil
	}
	stop() // Also cancel pending work when a listener failed, not only on signals.
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if e := srv.Shutdown(shutdown); e != nil {
		srv.Close()
	}
	if e := admin.Shutdown(shutdown); e != nil {
		admin.Close()
	}
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}
