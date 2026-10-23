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
	Task                 p.Task      `json:"task"`
	Report               p.Report    `json:"report"`
	MetadataAcknowledged bool        `json:"metadata_acknowledged,omitempty"`
	Acknowledged         bool        `json:"acknowledged"`
	Spool                *p.Manifest `json:"spool,omitempty"`
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
	writeJSON        func(string, any) error
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
	meter := telemetry.New()
	return &Node{connections: connections, uploadClient: uploadClient, meter: meter, writeJSON: meter.WriteJSON, config: config, client: client, origin: origin, instance: instance, runner: runner.Runner{WorkDir: config.WorkDir, BusyBox: config.BusyBox}, Logf: log.Printf}, nil
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
	if err := a.writeJSON(identityPath, identity); err != nil {
		return err
	}
	a.runner.Owner = a.instance
	if _, err := a.runner.Recover(); err != nil {
		return err
	}
	path := filepath.Join(a.config.StateDir, "journal.json")
	records, retainedFormat, err := visitJournal(path, func(rec journal) (journal, error) {
		if rec.Task.NodeID != a.config.NodeID {
			return rec, errors.New("journal belongs to another node")
		}
		return a.spoolRecord(rec)
	})
	if err != nil {
		return err
	}
	if records == nil {
		return errors.New("invalid null task journal")
	}
	if err := a.recoverReplay(); err != nil {
		return err
	}
	records, retainedFormat, err = a.recoverJournal(records, retainedFormat)
	if err != nil {
		return err
	}
	if err := a.recoverSpools(records); err != nil {
		return err
	}
	for _, rec := range records {
		if rec.Spool != nil {
			retainedFormat = true
		}
	}
	if retainedFormat {
		if _, err := os.Stat(a.replayPath()); err != nil {
			return errors.New("retained journal requires its replay checkpoint")
		}
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
	persist := func() error {
		return a.persistJournal(path, records, retainedFormat)
	}
	persistResult := func() error {
		retry := time.Second
		for {
			err := persist()
			if err == nil {
				return nil
			}
			if ctx.Err() != nil {
				return err
			}
			if a.Logf != nil {
				a.Logf("retaining result while journal persistence fails: %v", err)
			}
			if e := delay(ctx, retry); e != nil {
				return err
			}
			retry = nextRetry(retry)
		}
	}
	if err := persist(); err != nil {
		return err
	}
	if err := a.negotiate(ctx); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	high, err := a.openReplay(ctx, records)
	if err != nil {
		return err
	}
	if a.version >= p.LatestVersion {
		retainedFormat = true
		if err := persist(); err != nil {
			return err
		}
	}
	recordPeak := len(records)
	type completed struct {
		id, state string
		result    *p.Result
	}
	finished := make(chan completed, 1)
	type synced struct {
		response p.SyncResponse
		report   *p.Report
		err      error
		receipts []string
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
		if a.version < p.TransferVersion || uploading != "" {
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
				if rec.Spool != nil {
					v.Upload = rec.Spool
				}
				report = &v
				break
			}
		}
		if report == nil && active != "" {
			v := records[active].Report
			report = &v
		}
		metrics := a.nodeMetrics(records, active)
		var receipts []string
		if a.version >= p.LatestVersion {
			receipts = receiptBatch(records)
		}
		watermark := high
		callCtx, cancel := context.WithCancel(ctx)
		cancelSync = cancel
		interrupted = false
		workers.Add(1)
		go func() {
			defer workers.Done()
			response, err := a.syncRetention(callCtx, report, metrics, watermark, receipts)
			syncedCh <- synced{response: response, report: report, err: err, receipts: receipts}
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
			if err := persistResult(); err != nil {
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
			removedReceipts := false
			allowed := map[string]bool{}
			for _, id := range done.receipts {
				allowed[id] = true
			}
			for _, id := range response.Receipts {
				if !allowed[id] || !records[id].Acknowledged {
					return errors.New("invalid receipt acknowledgement")
				}
				delete(records, id)
				delete(allowed, id)
				removedReceipts = true
			}
			// Deleting map entries does not release their bucket storage. Shrink
			// large legacy journals geometrically, without rebuilding the small
			// normal-workload map on every completed task.
			if removedReceipts && recordPeak > 128 && len(records) <= recordPeak/4 {
				compact := make(map[string]journal, len(records))
				for id, rec := range records {
					compact[id] = rec
				}
				records, recordPeak = compact, len(compact)
			}
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
					if a.version >= p.LatestVersion {
						if task.Sequence == 0 || task.Sequence <= high {
							rec.Report.State = "unknown"
							rec.Report.Result = &p.Result{FinishedAt: time.Now().UTC(), Error: "task is behind the durable replay watermark; refusing execution"}
						} else {
							if err := a.saveReplay(task.Sequence); err != nil {
								return err
							}
							high = task.Sequence
						}
					}
					if response.CancelTaskID == task.ID && rec.Report.State == "running" {
						rec.Report.State = "canceled"
						rec.Report.Result = &p.Result{FinishedAt: time.Now().UTC()}
					}
					records[task.ID] = rec
					if len(records) > recordPeak {
						recordPeak = len(records)
					}
					if err := persist(); err != nil {
						return err
					}
					changed, removedReceipts = false, false
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
			// Receipt deletion only removes already-acknowledged tombstones.
			// Batch that optional cleanup until the receipts drain or another
			// durable transition writes the journal. A crash merely resends old
			// receipts; execution intent/results/checkpoints are still synchronous.
			if changed || (removedReceipts && !hasReceipts(records)) {
				if err := persist(); err != nil {
					return err
				}
			}
			startUpload()
			if a.version >= p.TransferVersion && response.CancelTaskID == "" {
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
				if rec.Spool != nil {
					m.PendingResultBytes += rec.Spool.Total()
				} else {
					m.PendingResultBytes += len(rec.Report.Result.Stdout) + len(rec.Report.Result.Stderr)
				}
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
	rec.Task = p.Task{ID: rec.Task.ID, Sequence: rec.Task.Sequence, NodeID: rec.Task.NodeID, CreatedAt: rec.Task.CreatedAt}
	rec.Report = p.Report{TaskID: rec.Report.TaskID, State: rec.Report.State, Result: &p.Result{Error: "result acknowledged by controller"}}
	return rec
}
