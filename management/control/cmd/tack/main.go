package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"tack.local/management/control"
	p "tack.local/management/protocol"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: tack provision|nodes|tasks|submit|get|cancel [options]")
	}
	if args[0] == "provision" {
		return provision(args[1:])
	}
	f := flag.NewFlagSet(args[0], flag.ContinueOnError)
	server := f.String("server", "", "controller HTTPS origin")
	configPath := f.String("config", "", "local controller JSON config supplying administrator token")
	ca := f.String("ca", "", "private CA file")
	dev := f.Bool("allow-insecure-loopback", false, "allow HTTP on a literal loopback address for development")
	id := f.String("id", "", "task ID")
	file := f.String("file", "", "task JSON file for submit")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	var config p.ControlConfig
	if err := p.ReadJSON(*configPath, &config); err != nil {
		return err
	}
	client, origin, err := p.HTTPClient(*server, *ca, *dev)
	if err != nil {
		return err
	}
	method, path := "GET", ""
	var body []byte
	switch args[0] {
	case "nodes":
		path = "/v1/nodes"
	case "tasks":
		path = "/v1/tasks"
	case "get", "cancel":
		if !p.ValidID(*id) {
			return errors.New("valid -id required")
		}
		path = "/v1/tasks/" + *id
		if args[0] == "cancel" {
			method = "POST"
			path += "/cancel"
		}
	case "submit":
		var task p.CreateTask
		if err := p.ReadJSON(*file, &task); err != nil {
			return err
		}
		task.Command.Defaults()
		if err := task.Command.Validate(); err != nil {
			return err
		}
		body, err = json.Marshal(task)
		if err != nil {
			return err
		}
		method, path = "POST", "/v1/tasks"
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
	req, err := http.NewRequest(method, origin+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+config.AdminToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16*p.MaxBody))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("controller returned %d: %s", resp.StatusCode, b)
	}
	_, err = os.Stdout.Write(b)
	return err
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
