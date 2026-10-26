package command

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
	"tack.local/management/protocol/rotatelog"
)

func Main() {
	MainWithRunner(RunNode)
}

// MainWithRunner shares configuration, logging and signal handling with the
// unified binary without importing modern tunnel dependencies into this module.
func MainWithRunner(runNode func(context.Context, p.NodeConfig, bool) error) {
	if err := run(runNode); err != nil {
		log.Fatal(err)
	}
}
func run(runNode func(context.Context, p.NodeConfig, bool) error) error {
	configPath := flag.String("config", "", "Node JSON configuration (required)")
	version := flag.Bool("version", false, "print release version")
	logs := rotatelog.Flags(flag.CommandLine)
	flag.Parse()
	if *version {
		fmt.Println(p.BuildVersion)
		return nil
	}
	closeLog, err := logs.Install()
	if err != nil {
		return err
	}
	defer closeLog()
	if *configPath == "" {
		return fmt.Errorf("-config is required; deployment and startup registration are external")
	}
	var config p.NodeConfig
	if err := p.ReadJSON(*configPath, &config); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Printf("tack-node %s starting in foreground", config.NodeID)
	return runNode(ctx, config, logs.Enabled())
}

// RunNode runs the management-only development worker. A tunnel configuration
// requires the unified binary and is rejected by node.New rather than ignored.
func RunNode(ctx context.Context, config p.NodeConfig, logging bool) error {
	a, err := node.New(config)
	if err != nil {
		return err
	}
	if !logging {
		a.Logf = nil
	}
	return a.Run(ctx)
}
