package node

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"tack.local/management/node/runner"
	p "tack.local/management/protocol"
	"tack.local/management/protocol/telemetry"
)

type journal struct {
	Task                 p.Task   `json:"task"`
	Report               p.Report `json:"report"`
	MetadataAcknowledged bool     `json:"metadata_acknowledged,omitempty"`
	Acknowledged         bool     `json:"acknowledged"`
}
type Node struct {
	config           p.NodeConfig
	client           *http.Client
	uploadClient     *http.Client
	meter            *telemetry.Meter
	connections      *connectionSet
	version          int
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
	if config.HeartbeatSeconds == 0 {
		config.HeartbeatSeconds = 1800
	}
	if config.HeartbeatSeconds < 2 || config.HeartbeatSeconds > p.MaxHeartbeatSeconds {
		return nil, errors.New("heartbeat_seconds must be 2..86400")
	}
	if config.LeaseSeconds == 0 {
		config.LeaseSeconds = config.HeartbeatSeconds * 5
		if config.LeaseSeconds < 90 {
			config.LeaseSeconds = 90
		}
	}
	if config.LeaseSeconds < config.HeartbeatSeconds*3/2+30 || config.LeaseSeconds > 604800 {
		return nil, errors.New("lease_seconds must cover the maximum jittered heartbeat plus 30s, and be at most 604800")
	}
	if config.MaxPendingResults == 0 {
		config.MaxPendingResults = 8
	}
	if config.MaxPendingResultBytes == 0 {
		config.MaxPendingResultBytes = 8 * p.MaxOutput
	}
	if config.MaxPendingResults < 1 || config.MaxPendingResults > 128 || config.MaxPendingResultBytes < 2*p.MaxOutput || config.MaxPendingResultBytes > 128*p.MaxOutput {
		return nil, errors.New("invalid pending result limits")
	}
	if config.UploadBytesPerSecond < 0 || config.UploadBytesPerSecond > 1024*p.MaxOutput {
		return nil, errors.New("invalid upload_bytes_per_second")
	}
	client, origin, err := p.HTTPClient(config.Server, config.CAFile, config.AllowInsecureLoopback)
	if err != nil {
		return nil, err
	}
	instance, err := p.RandomID()
	if err != nil {
		return nil, err
	}
	uploadClient, _, err := p.HTTPClient(config.Server, config.CAFile, config.AllowInsecureLoopback)
	if err != nil {
		return nil, err
	}
	uploadClient.Timeout = time.Duration(p.ResultTimeoutSeconds+15) * time.Second
	connections := newConnectionSet()
	connections.attach(client)
	connections.attach(uploadClient)
	return &Node{connections: connections, uploadClient: uploadClient, meter: telemetry.New(), config: config, client: client, origin: origin, instance: instance, runner: runner.Runner{WorkDir: config.WorkDir, BusyBox: config.BusyBox}, Logf: log.Printf}, nil
}

// Run is a foreground worker. It never installs services or selects deployment paths.
func (a *Node) Run(parent context.Context) (runErr error) {
	if a.connections.isClosed() {
		return errors.New("node has stopped; create a new Node to restart")
	}
	unlock, err := p.Lock(filepath.Join(a.config.StateDir, "agent.lock"))
	if err != nil {
		return err
	}
	defer unlock()
	ctx, stop := context.WithCancel(parent)
	var workers sync.WaitGroup
	defer func() {
		stop()
		a.connections.close()
		workers.Wait()
		a.client.CloseIdleConnections()
		a.uploadClient.CloseIdleConnections()
	}()
	identityPath := filepath.Join(a.config.StateDir, "identity.json")
	var identity struct {
		NodeID     string `json:"node_id"`
		InstanceID string `json:"instance_id,omitempty"`
	}
	if err := p.ReadJSON(identityPath, &identity); err != nil && !os.IsNotExist(err) {
		return err
	}
	if identity.NodeID != "" && identity.NodeID != a.config.NodeID {
		return errors.New("state directory belongs to a different node")
	}
	if identity.InstanceID != "" {
		if !p.ValidID(identity.InstanceID) {
			return errors.New("invalid persisted instance identity")
		}
		a.instance = identity.InstanceID
	}
	identity.NodeID, identity.InstanceID = a.config.NodeID, a.instance
	if err := a.meter.WriteJSON(identityPath, identity); err != nil {
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
			rec.MetadataAcknowledged, rec.Acknowledged = false, false
			records[id] = rec
		}
		if rec.Acknowledged {
			records[id] = acknowledge(rec)
		}
		if p.Terminal(rec.Report.State) && rec.Report.Result == nil {
			return errors.New("terminal journal entry has no result")
		}
	}
	persist := func() error { return a.meter.WriteJSON(path, records) }
	if err := persist(); err != nil {
		return err
	}
	if err := a.negotiate(ctx); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	type completed struct {
		id, state string
		result    *p.Result
	}
	finished := make(chan completed, 1)
	type synced struct {
		response p.SyncResponse
		report   *p.Report
		err      error
	}
	syncedCh := make(chan synced, 1)
	uploaded := make(chan string, 1)
	var active, uploading string
	var cancelJob, cancelSync context.CancelFunc
	interrupted := false
	// Only this goroutine owns records. Background work uses immutable snapshots.
	defer func() {
		if cancelJob != nil {
			cancelJob()
			done := <-finished
			rec := records[done.id]
			rec.Report = p.Report{TaskID: done.id, State: done.state, Result: done.result}
			records[done.id] = rec
			if err := persist(); err != nil {
				runErr = err
			}
		}
	}()
	startUpload := func() {
		if a.version != p.LatestVersion || uploading != "" {
			return
		}
		var selected *journal
		for _, rec := range records {
			if rec.MetadataAcknowledged && !rec.Acknowledged && p.Terminal(rec.Report.State) && (selected == nil || rec.Task.CreatedAt.Before(selected.Task.CreatedAt)) {
				v := rec
				selected = &v
			}
		}
		if selected == nil {
			return
		}
		uploading = selected.Task.ID
		job := *selected
		workers.Add(1)
		go func() {
			defer workers.Done()
			if a.upload(ctx, job) == nil {
				uploaded <- job.Task.ID
			}
		}()
	}
	startSync := func() {
		if cancelSync != nil {
			return
		}
		var report *p.Report
		for _, rec := range records {
			if !rec.Acknowledged && p.Terminal(rec.Report.State) && (a.version == p.Version || !rec.MetadataAcknowledged) {
				v := rec.Report
				report = &v
				break
			}
		}
		if report == nil && active != "" {
			v := records[active].Report
			report = &v
		}
		metrics := a.nodeMetrics(records, active)
		callCtx, cancel := context.WithCancel(ctx)
		cancelSync = cancel
		interrupted = false
		workers.Add(1)
		go func() {
			defer workers.Done()
			response, err := a.sync(callCtx, report, metrics)
			syncedCh <- synced{response: response, report: report, err: err}
		}()
	}
	interruptSync := func() {
		if cancelSync != nil {
			interrupted = true
			cancelSync()
		} else {
			startSync()
		}
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	resetTimer := func(d time.Duration) {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(d)
	}
	retry := time.Second
	startUpload()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
			startSync()
		case done := <-finished:
			rec := records[done.id]
			rec.Report = p.Report{TaskID: done.id, State: done.state, Result: done.result}
			records[done.id] = rec
			cancelJob()
			active, cancelJob = "", nil
			if err := persist(); err != nil {
				return err
			}
			interruptSync()
		case id := <-uploaded:
			rec := records[id]
			rec = acknowledge(rec)
			records[id] = rec
			uploading = ""
			if err := persist(); err != nil {
				return err
			}
			startUpload()
			interruptSync()
		case done := <-syncedCh:
			wasInterrupted := interrupted
			cancelSync()
			cancelSync = nil
			if ctx.Err() != nil {
				return nil
			}
			if done.err != nil {
				if wasInterrupted {
					startSync()
					continue
				}
				if a.Logf != nil {
					a.Logf("management connection: %v", done.err)
				}
				resetTimer(retryDelay(retry))
				retry = nextRetry(retry)
				continue
			}
			retry = time.Second
			response, report := done.response, done.report
			changed := false
			if report != nil && response.Acknowledged == report.TaskID {
				rec := records[report.TaskID]
				if p.Terminal(rec.Report.State) && p.Terminal(report.State) {
					if a.version == p.Version {
						rec = acknowledge(rec)
					} else {
						rec.MetadataAcknowledged = true
					}
					records[report.TaskID], changed = rec, true
				}
			}
			if active != "" && response.CancelTaskID == active {
				cancelJob()
			}
			if task := response.Task; task != nil && active == "" {
				if !p.ValidID(task.ID) || task.NodeID != a.config.NodeID {
					return errors.New("controller returned invalid task identity")
				}
				if existing, ok := records[task.ID]; ok {
					if existing.Acknowledged {
						existing.Report.State = "unknown"
						existing.Report.Result = &p.Result{FinishedAt: time.Now().UTC(), Error: "task was already acknowledged; refusing replay"}
						existing.Acknowledged, existing.MetadataAcknowledged = false, false
					}
					existing.MetadataAcknowledged = false
					records[task.ID], changed = existing, true
				} else if a.version == p.Version || a.nodeMetrics(records, active).AcceptingTasks {
					if len(records) >= 10000 {
						return errors.New("journal capacity reached; retention upgrade required")
					}
					rec := journal{Task: *task, Report: p.Report{TaskID: task.ID, State: "running"}}
					if response.CancelTaskID == task.ID {
						rec.Report.State = "canceled"
						rec.Report.Result = &p.Result{FinishedAt: time.Now().UTC()}
					}
					records[task.ID] = rec
					if err := persist(); err != nil {
						return err
					}
					if rec.Report.State == "running" {
						jobCtx, cancel := context.WithCancel(ctx)
						active, cancelJob = task.ID, cancel
						go func(t p.Task) {
							state, result := a.runner.Run(jobCtx, t.Command)
							finished <- completed{id: t.ID, state: state, result: result}
						}(*task)
					}
				}
			}
			if changed {
				if err := persist(); err != nil {
					return err
				}
			}
			startUpload()
			if a.version == p.LatestVersion && response.CancelTaskID == "" {
				resetTimer(0)
			} else {
				resetTimer(time.Duration(a.config.PollSeconds) * time.Second)
			}
		}
	}
}

func (a *Node) nodeMetrics(records map[string]journal, active string) p.NodeMetrics {
	m := p.NodeMetrics{Runtime: a.meter.Snapshot(), JournalRecords: len(records)}
	for _, rec := range records {
		if !rec.Acknowledged && p.Terminal(rec.Report.State) {
			m.PendingResults++
			if rec.Report.Result != nil {
				m.PendingResultBytes += len(rec.Report.Result.Stdout) + len(rec.Report.Result.Stderr)
			}
		}
	}
	// Reserve a maximum-sized result before starting another task.
	m.AcceptingTasks = active == "" && len(records) < 10000 && m.PendingResults < a.config.MaxPendingResults && m.PendingResultBytes <= a.config.MaxPendingResultBytes-2*p.MaxOutput
	return m
}

// Completed records need identities, not the original command or payload. Keep
// tombstones to suppress replay without retaining scripts, arguments and env.
func acknowledge(rec journal) journal {
	rec.Acknowledged = true
	rec.Task = p.Task{ID: rec.Task.ID, NodeID: rec.Task.NodeID, CreatedAt: rec.Task.CreatedAt}
	rec.Report = p.Report{TaskID: rec.Report.TaskID, State: rec.Report.State, Result: &p.Result{Error: "result acknowledged by controller"}}
	return rec
}
