package main

import (
	"os"
	"path/filepath"
	"testing"

	p "tack.local/management/protocol"
)

func TestProvisionSeparatesCredentialsAndPreservesExistingNodes(t *testing.T) {
	dir := t.TempDir()
	controllerPath := filepath.Join(dir, "controller.json")
	provisionNode := func(id, output string) error {
		return provision([]string{"-config", controllerPath, "-node-config", output,
			"-node", id, "-server", "https://controller.example",
			"-state-dir", `C:\Customer Data\state`, "-work-dir", `C:\Customer Data\work`})
	}
	firstPath, secondPath := filepath.Join(dir, "first.json"), filepath.Join(dir, "second.json")
	if err := provisionNode("node-one", firstPath); err != nil {
		t.Fatal(err)
	}
	var first p.NodeConfig
	var original p.ControlConfig
	if err := p.ReadJSON(firstPath, &first); err != nil {
		t.Fatal(err)
	}
	if err := p.ReadJSON(controllerPath, &original); err != nil {
		t.Fatal(err)
	}
	if first.Token != original.Nodes[0].Token || first.Token == original.AdminToken || first.StateDir != `C:\Customer Data\state` {
		t.Fatal("node credentials or target-specific paths were not preserved")
	}
	if err := provisionNode("node-two", secondPath); err != nil {
		t.Fatal(err)
	}
	var updated p.ControlConfig
	if err := p.ReadJSON(controllerPath, &updated); err != nil {
		t.Fatal(err)
	}
	if updated.AdminToken != original.AdminToken || len(updated.Nodes) != 2 || updated.Nodes[0] != original.Nodes[0] || updated.Nodes[1].Token == first.Token {
		t.Fatal("existing node or administrator credentials changed")
	}
	before, err := os.ReadFile(controllerPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := provisionNode("node-one", filepath.Join(dir, "duplicate.json")); err == nil {
		t.Fatal("duplicate identity accepted")
	}
	if err := provisionNode("node-three", firstPath); err == nil {
		t.Fatal("existing output overwritten")
	}
	after, err := os.ReadFile(controllerPath)
	if err != nil || string(before) != string(after) {
		t.Fatal("failed provision changed controller configuration")
	}
}
