package node

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	p "tack.local/management/protocol"
)

func TestServiceFailureStopsOfflineNodeAndReleasesLock(t *testing.T) {
	cfg := testConfig(t)
	cfg.Server = "http://127.0.0.1:1"
	cfg.AllowInsecureLoopback = true
	a, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := errors.New("service failed")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := a.RunWithService(ctx, func(context.Context) error { return want }); !errors.Is(err, want) {
		t.Fatalf("service failure lost: %v", err)
	}
	unlock, err := p.Lock(filepath.Join(cfg.StateDir, "agent.lock"))
	if err != nil {
		t.Fatal("Node lock leaked:", err)
	}
	unlock()
}

func TestDuplicateNodeDoesNotStartService(t *testing.T) {
	cfg := testConfig(t)
	unlock, err := p.Lock(filepath.Join(cfg.StateDir, "agent.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	a, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	started := false
	err = a.RunWithService(context.Background(), func(context.Context) error { started = true; return nil })
	if err == nil || started {
		t.Fatalf("duplicate Node started its service: started=%v err=%v", started, err)
	}
}

func TestManagementOnlyWorkerRejectsTunnel(t *testing.T) {
	cfg := testConfig(t)
	cfg.Tunnel = &p.TunnelConfig{}
	if _, err := New(cfg); err == nil {
		t.Fatal("management-only worker silently accepted a tunnel")
	}
}
