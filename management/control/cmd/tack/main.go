package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"tack.local/management/control"
	p "tack.local/management/protocol"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(exitCode(err))
	}
}

func provision(args []string) error {
	f := flag.NewFlagSet("provision", flag.ContinueOnError)
	configPath := f.String("config", "", "controller config to create or extend (restart controller after changes)")
	nodePath := f.String("node-config", "", "new output file containing this node's credential")
	node := f.String("node", "", "stable unique node ID")
	server := f.String("server", "", "controller HTTPS origin")
	stateDir := f.String("state-dir", "", "node-side state directory")
	workDir := f.String("work-dir", "", "node-side task workspace directory")
	persistence := f.String("state-persistence", "durable", "durable or volatile; describes deployment storage")
	ca := f.String("node-ca", "", "optional node-side private CA path")
	f.StringVar(nodePath, "agent-config", "", "deprecated alias for -node-config")
	f.StringVar(ca, "agent-ca", "", "deprecated alias for -node-ca")
	busybox := f.String("busybox", "", "optional node-side BusyBox absolute path")
	tunnelPath := f.String("tunnel-config", "", "optional local JSON file with this node's embedded Chisel configuration")
	dev := f.Bool("allow-insecure-loopback", false, "development HTTP on loopback")
	if err := f.Parse(args); err != nil {
		return err
	}
	if !p.ValidID(*node) || *configPath == "" || *nodePath == "" || *stateDir == "" || *workDir == "" {
		return errors.New("config, node-config, node, state-dir and work-dir are required")
	}
	if *persistence != "durable" && *persistence != "volatile" {
		return errors.New("invalid state-persistence")
	}
	if _, _, err := p.HTTPClient(*server, "", *dev); err != nil {
		return err
	}
	var tunnel *p.TunnelConfig
	if *tunnelPath != "" {
		tunnel = new(p.TunnelConfig)
		if err := p.ReadJSON(*tunnelPath, tunnel); err != nil {
			return err
		}
		if _, _, err := p.HTTPClient(tunnel.Server, "", *dev); err != nil {
			return fmt.Errorf("tunnel server: %w", err)
		}
	}
	a, _ := filepath.Abs(*configPath)
	b, _ := filepath.Abs(*nodePath)
	if a == b {
		return errors.New("controller and node config paths must differ")
	}
	unlock, err := p.Lock(a + ".lock")
	if err != nil {
		return err
	}
	defer unlock()
	if _, err := os.Lstat(*nodePath); !os.IsNotExist(err) {
		return errors.New("node config destination must not exist")
	}
	var config p.ControlConfig
	if err := p.ReadJSON(*configPath, &config); err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		token, err := p.RandomID()
		if err != nil {
			return err
		}
		config.AdminToken = token
	}
	token, err := p.RandomID()
	if err != nil {
		return err
	}
	config.Nodes = append(config.Nodes, p.NodeCredential{ID: *node, Token: token})
	if err := control.ValidateConfig(config); err != nil {
		return err
	}
	nodeConfig := p.NodeConfig{Server: *server, NodeID: *node, Token: token, StateDir: *stateDir, WorkDir: *workDir, StatePersistence: *persistence, CAFile: *ca, BusyBox: *busybox, AllowInsecureLoopback: *dev}
	nodeConfig.Tunnel = tunnel
	if err := p.WriteJSON(*nodePath, nodeConfig); err != nil {
		return err
	}
	if err := p.WriteJSON(*configPath, config); err != nil {
		os.Remove(*nodePath)
		return err
	}
	fmt.Printf("Provisioned %s. Transfer %s privately; restart the controller to load %s.\n", *node, *nodePath, *configPath)
	return nil
}
