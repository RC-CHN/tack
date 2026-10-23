package node

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	p "tack.local/management/protocol"
)

func TestAcknowledgementReleasesCommandAndOutput(t *testing.T) {
	for _, version := range []int{p.Version, p.LatestVersion} {
		name := "v1"
		if version == p.LatestVersion {
			name = "v2"
		}
		t.Run(name, func(t *testing.T) {
			cfg := testConfig(t)
			cfg.PollSeconds = 1
			task := p.Task{ID: "done", NodeID: cfg.NodeID, Command: p.Command{Mode: "posix-sh", Script: strings.Repeat("# padding\n", 24000), Env: map[string]string{"padding": strings.Repeat("x", 32768)}}}
			rec := journal{Task: task, Report: p.Report{TaskID: task.ID, State: "succeeded", Result: &p.Result{Stdout: []byte("retained until final ack")}}}
			path := filepath.Join(cfg.StateDir, "journal.json")
			if err := p.WriteJSON(path, map[string]journal{task.ID: rec}); err != nil {
				t.Fatal(err)
			}
			a, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			observed := false
			a.client = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/health" {
					return response(map[string]int{"protocol_version": version}), nil
				}
				var in p.SyncRequest
				if err := p.Decode(r.Body, &in); err != nil {
					t.Error(err)
				}
				out := p.SyncResponse{Version: version}
				if in.Report != nil {
					out.Acknowledged = in.Report.TaskID
				}
				if in.Report == nil && (version == p.Version || in.Node.Metrics.PendingResults == 0) {
					observed = true
					cancel()
				}
				return response(out), nil
			})}
			a.uploadClient = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
				var in p.ResultChunk
				p.Decode(r.Body, &in)
				return response(p.ChunkAck{Version: p.LatestVersion, TaskID: task.ID, Offset: len(rec.Report.Result.Stdout), Complete: true}), nil
			})}
			if err := a.Run(ctx); err != nil {
				t.Fatal(err)
			}
			if !observed {
				t.Fatal("final acknowledgement was not observed")
			}
			records := map[string]journal{}
			if err := p.ReadJSON(path, &records); err != nil {
				t.Fatal(err)
			}
			saved := records[task.ID]
			if !saved.Acknowledged || saved.Task.ID != task.ID || saved.Task.NodeID != cfg.NodeID || saved.Report.TaskID != task.ID || saved.Report.State != "succeeded" {
				t.Fatal("lost replay identity")
			}
			if saved.Task.Command.Script != "" || len(saved.Task.Command.Env) != 0 || len(saved.Report.Result.Stdout) != 0 {
				t.Fatal("acknowledged record retained large payload")
			}
		})
	}
}

func TestStartupCompactsOldAcknowledgedRecords(t *testing.T) {
	cfg := testConfig(t)
	path := filepath.Join(cfg.StateDir, "journal.json")
	rec := journal{Task: p.Task{ID: "old", NodeID: cfg.NodeID, Command: p.Command{Script: strings.Repeat("x", 250000)}}, Report: p.Report{TaskID: "old", State: "failed", Result: &p.Result{}}, Acknowledged: true}
	if err := p.WriteJSON(path, map[string]journal{"old": rec}); err != nil {
		t.Fatal(err)
	}
	a, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.client = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		cancel()
		return response(map[string]int{"protocol_version": 1}), nil
	})}
	if err := a.Run(ctx); err != nil {
		t.Fatal(err)
	}
	var records map[string]journal
	if err := p.ReadJSON(path, &records); err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || !records["old"].Acknowledged || records["old"].Task.Command.Script != "" {
		t.Fatal("old record was not compacted safely")
	}
}
