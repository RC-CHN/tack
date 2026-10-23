package node

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"tack.local/management/node/runner"
	p "tack.local/management/protocol"
)

type journal struct {
	Task         p.Task   `json:"task"`
	Report       p.Report `json:"report"`
	Acknowledged bool     `json:"acknowledged"`
}
type Node struct {
	config           p.NodeConfig
	client           *http.Client
	origin, instance string
	runner           runner.Runner
	Logf             func(string, ...any)
}

func New(config p.NodeConfig) (*Node, error) {
	if !p.ValidID(config.NodeID) || len(config.Token) < 32 {
		return nil, errors.New("valid node_id and token are required")
	}
	if !filepath.IsAbs(config.StateDir) || !filepath.IsAbs(config.WorkDir) {
		return nil, errors.New("state_dir and work_dir must be absolute externally provisioned paths")
	}
	for _, path := range []string{config.StateDir, config.WorkDir} {
		info, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("%s is not a directory", path)
		}
	}
	if config.StatePersistence == "" {
		config.StatePersistence = "durable"
	}
	if config.StatePersistence != "durable" && config.StatePersistence != "volatile" {
		return nil, errors.New("state_persistence must be durable or volatile")
	}
	if config.PollSeconds == 0 {
		config.PollSeconds = 2
	}
	if config.PollSeconds < 1 || config.PollSeconds > 10 {
		return nil, errors.New("poll_seconds must be 1..10")
	}
	client, origin, err := p.HTTPClient(config.Server, config.CAFile, config.AllowInsecureLoopback)
	if err != nil {
		return nil, err
	}
	instance, err := p.RandomID()
	if err != nil {
		return nil, err
	}
	return &Node{config: config, client: client, origin: origin, instance: instance, runner: runner.Runner{WorkDir: config.WorkDir, BusyBox: config.BusyBox}, Logf: log.Printf}, nil
}

func (a *Node) sync(ctx context.Context, report *p.Report) (p.SyncResponse, error) {
	host, _ := os.Hostname()
	input := p.SyncRequest{Version: p.Version, InstanceID: a.instance, Node: p.Node{ID: a.config.NodeID, Hostname: host, OS: runtime.GOOS, Arch: runtime.GOARCH, Modes: a.runner.Modes(), StatePersistence: a.config.StatePersistence}, Report: report}
	b, err := json.Marshal(input)
	if err != nil {
		return p.SyncResponse{}, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", a.origin+p.NodeSyncPath, bytes.NewReader(b))
	if err != nil {
		return p.SyncResponse{}, err
	}
	req.Header.Set("Authorization", "Bearer "+a.config.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		return p.SyncResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return p.SyncResponse{}, fmt.Errorf("controller sync returned HTTP %d", resp.StatusCode)
	}
	var output p.SyncResponse
	if err := p.Decode(io.LimitReader(resp.Body, p.MaxBody), &output); err != nil {
		return output, err
	}
	if output.Version != p.Version {
		return output, errors.New("unsupported controller protocol version")
	}
	return output, nil
}

// Run is a foreground worker. It never installs services or selects deployment paths.
func (a *Node) Run(ctx context.Context) (runErr error) {
	// Keep the existing lock name so pre-rename and Tack processes cannot
	// concurrently use the same identity and execution journal.
	unlock, err := p.Lock(filepath.Join(a.config.StateDir, "agent.lock"))
	if err != nil {
		return err
	}
	defer unlock()
	identityPath := filepath.Join(a.config.StateDir, "identity.json")
	var identity struct {
		NodeID string `json:"node_id"`
	}
	if err := p.ReadJSON(identityPath, &identity); err != nil && !os.IsNotExist(err) {
		return err
	}
	if identity.NodeID != "" && identity.NodeID != a.config.NodeID {
		return errors.New("state directory belongs to a different node")
	}
	identity.NodeID = a.config.NodeID
	if err := p.WriteJSON(identityPath, identity); err != nil {
		return err
	}
	path := filepath.Join(a.config.StateDir, "journal.json")
	records := map[string]journal{}
	if err := p.ReadJSON(path, &records); err != nil && !os.IsNotExist(err) {
		return err
	}
	if records == nil {
		return errors.New("invalid null task journal")
	}
	for id, rec := range records {
		if !p.ValidID(id) || rec.Task.ID != id || rec.Report.TaskID != id || rec.Task.NodeID != a.config.NodeID {
			return errors.New("task journal contains invalid or mismatched identities")
		}
		if !p.Terminal(rec.Report.State) {
			rec.Report.State = "unknown"
			rec.Report.Result = &p.Result{FinishedAt: time.Now().UTC(), Error: "node restarted after execution intent; task will not be replayed"}
			records[id] = rec
		}
	}
	if err := p.WriteJSON(path, records); err != nil {
		return err
	}
	type completed struct {
		id, state string
		result    *p.Result
	}
	finished := make(chan completed, 1)
	var active string
	var cancel context.CancelFunc
	defer func() {
		if cancel != nil {
			cancel()
			done := <-finished
			rec := records[done.id]
			rec.Report = p.Report{TaskID: done.id, State: done.state, Result: done.result}
			records[done.id] = rec
			if err := p.WriteJSON(path, records); err != nil {
				runErr = err
			}
		}
	}()
	ticker := time.NewTicker(time.Duration(a.config.PollSeconds) * time.Second)
	defer ticker.Stop()
	for {
		select {
		case done := <-finished:
			rec := records[done.id]
			rec.Report = p.Report{TaskID: done.id, State: done.state, Result: done.result}
			records[done.id] = rec
			cancel()
			active = ""
			cancel = nil
			if err := p.WriteJSON(path, records); err != nil {
				return err
			}
		default:
		}
		var report *p.Report
		if active != "" {
			v := records[active].Report
			report = &v
		} else {
			for _, rec := range records {
				if !rec.Acknowledged {
					v := rec.Report
					report = &v
					break
				}
			}
		}
		response, err := a.sync(ctx, report)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if a.Logf != nil {
				a.Logf("management connection: %v", err)
			}
		} else {
			if response.Acknowledged != "" && report != nil && response.Acknowledged == report.TaskID {
				rec := records[report.TaskID]
				if p.Terminal(rec.Report.State) {
					rec.Acknowledged = true
					rec.Report.Result = &p.Result{Error: "result acknowledged by controller"}
					records[report.TaskID] = rec
					if err := p.WriteJSON(path, records); err != nil {
						return err
					}
				}
			}
			if active != "" && response.CancelTaskID == active {
				cancel()
			}
			if task := response.Task; task != nil && active == "" {
				if !p.ValidID(task.ID) || task.NodeID != a.config.NodeID {
					return errors.New("controller returned invalid task identity")
				}
				if existing, ok := records[task.ID]; ok {
					// A stale/replayed delivery cannot execute an already journaled task.
					if existing.Acknowledged {
						existing.Report.State = "unknown"
						existing.Report.Result = &p.Result{FinishedAt: time.Now().UTC(), Error: "task was completed and acknowledged previously; retained result is on the controller; refusing replay"}
					}
					existing.Acknowledged = false
					records[task.ID] = existing
				} else {
					if len(records) >= 10000 {
						return errors.New("journal capacity reached; a retention upgrade is required to accept more tasks")
					}
					rec := journal{Task: *task, Report: p.Report{TaskID: task.ID, State: "running"}}
					if response.CancelTaskID == task.ID {
						rec.Report.State = "canceled"
						rec.Report.Result = &p.Result{FinishedAt: time.Now().UTC()}
					}
					records[task.ID] = rec
					if err := p.WriteJSON(path, records); err != nil {
						return err
					}
					if rec.Report.State == "running" {
						jobCtx, stop := context.WithCancel(ctx)
						active, cancel = task.ID, stop
						go func(t p.Task) {
							state, result := a.runner.Run(jobCtx, t.Command)
							finished <- completed{id: t.ID, state: state, result: result}
						}(*task)
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
