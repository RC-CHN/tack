package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"tack.local/management/node"
	p "tack.local/management/protocol"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}
func run() error {
	configPath := flag.String("config", "", "Node JSON configuration (required)")
	version := flag.Bool("version", false, "print release version")
	flag.Parse()
	if *version {
		fmt.Println(p.BuildVersion)
		return nil
	}
	if *configPath == "" {
		return fmt.Errorf("-config is required; deployment and startup registration are external")
	}
	var config p.NodeConfig
	if err := p.ReadJSON(*configPath, &config); err != nil {
		return err
	}
	a, err := node.New(config)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Printf("tack-node %s starting in foreground", config.NodeID)
	return a.Run(ctx)
}
