package control

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	p "tack.local/management/protocol"
	"tack.local/management/protocol/telemetry"
)

type state struct {
	Version   int               `json:"storage_version"`
	Tasks     map[string]p.Task `json:"tasks"`
	Sequences map[string]uint64 `json:"sequences,omitempty"`
}
type liveNode struct {
	info     p.Node
	instance string
}

// Server is a single-controller durable task queue. Run one process per state file.
type Server struct {
	tx           sync.Mutex // Serializes durable mutations; readers do not wait on disk writes.
	mu           sync.Mutex
	config       p.ControlConfig
	path         string
	state        state
	nodes        map[string]liveNode
	meter        *telemetry.Meter
	uploads      [64]sync.Mutex
	changed      chan struct{}
	historyBytes int64
	historyTasks int
	orphanBytes  int64
	orphanFiles  int
	evictions    evictionCache // tx protects this bounded metadata cache.
	writeJSON    func(string, any) error
	controlSlots map[string]chan struct{}
	uploadSlots  chan struct{}
	historySlots chan struct{}
	adminSlots   chan struct{}
}

func ValidateConfig(c p.ControlConfig) error {
	if err := storageDefaults(&c); err != nil {
		return err
	}
	if len(c.AdminToken) < 32 {
		return errors.New("admin_token must contain at least 32 characters")
	}
	ids, tokens := map[string]bool{}, map[string]bool{c.AdminToken: true}
	for _, n := range c.Nodes {
		if !p.ValidID(n.ID) || ids[n.ID] || len(n.Token) < 32 || tokens[n.Token] {
			return errors.New("node IDs and tokens must be valid and unique; tokens must have at least 32 characters")
		}
		ids[n.ID], tokens[n.Token] = true, true
	}
	return nil
}

func New(config p.ControlConfig, path string) (*Server, error) {
	if err := ValidateConfig(config); err != nil {
		return nil, err
	}
	storageDefaults(&config)
	s := &Server{config: config, path: path, state: state{Version: 2, Tasks: map[string]p.Task{}, Sequences: map[string]uint64{}}, nodes: map[string]liveNode{}, meter: telemetry.New(), changed: make(chan struct{})}
	s.writeJSON = s.meter.WriteJSON
	s.controlSlots = make(map[string]chan struct{}, len(config.Nodes))
	for _, n := range config.Nodes {
		s.controlSlots[n.ID] = make(chan struct{}, 2)
	}
	s.uploadSlots = make(chan struct{}, config.MaxConcurrentUploads)
	s.historySlots = make(chan struct{}, 8)
	s.adminSlots = make(chan struct{}, 4)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	if err := s.openStorage(); err != nil {
		return nil, err
	}
	if err := s.writeJSON(path, s.state); err != nil {
		return nil, err
	}
	if err := s.cleanUploads(); err != nil {
		return nil, err
	}
	if err := s.Maintain(); err != nil {
		return nil, err
	}
	return s, nil
}

func equalToken(a, b string) bool {
	x, y := sha256.Sum256([]byte(a)), sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(x[:], y[:]) == 1
}
func (s *Server) nodeExists(id string) bool {
	for _, n := range s.config.Nodes {
		if n.ID == id {
			return true
		}
	}
	return false
}

func respond(w http.ResponseWriter, code int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(value)
}
func failure(w http.ResponseWriter, code int, msg string) {
	respond(w, code, map[string]string{"error": msg})
}

// NodeHandler exposes no administrator routes, even with an administrator token.
func (s *Server) NodeHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !nodePath(r.URL.Path) && r.URL.Path != "/health" {
			failure(w, 404, "endpoint not found")
			return
		}
		s.ServeHTTP(w, r)
	})
}

func (s *Server) AdminHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if nodePath(r.URL.Path) {
			failure(w, 404, "endpoint not found")
			return
		}
		s.ServeHTTP(w, r)
	})
}
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := p.Decode(http.MaxBytesReader(w, r.Body, p.MaxBody), v); err != nil {
		failure(w, 400, "invalid request: "+err.Error())
		return false
	}
	return true
}

func nodePath(path string) bool {
	return path == p.NodeSyncPath || path == p.NodeControlPath || path == p.NodeRetentionPath || path == p.NodeResultPath
}
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if nodePath(r.URL.Path) {
		start := time.Now()
		status := &statusWriter{ResponseWriter: w, code: 200}
		defer func() {
			kind := ""
			if r.Context().Err() != nil {
				kind = "canceled"
			} else if status.code >= 400 {
				kind = "http"
			}
			s.meter.Request(r.URL.Path == p.NodeResultPath, start, status.code, kind)
		}()
		w = status
	}

	if r.URL.Path == "/health" && r.Method == "GET" {
		respond(w, 200, map[string]int{"protocol_version": p.LatestVersion})
		return
	}
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, "Bearer ") {
		failure(w, 401, "authentication required")
		return
	}
	token := strings.TrimPrefix(header, "Bearer ")
	if nodePath(r.URL.Path) {
		if r.Method != "POST" {
			failure(w, 405, "POST required")
			return
		}
		for _, n := range s.config.Nodes {
			if equalToken(token, n.Token) {
				slots := s.controlSlots[n.ID]
				if r.URL.Path == p.NodeResultPath {
					slots = s.uploadSlots
				}
				if !takeSlot(w, slots) {
					return
				}
				defer func() { <-slots }()
				if r.URL.Path == p.NodeResultPath {
					s.uploadResult(w, r, n.ID)
				} else {
					s.syncNode(w, r, n.ID)
				}
				return
			}
		}
		failure(w, 401, "invalid node credential")
		return
	}
	if !equalToken(token, s.config.AdminToken) {
		failure(w, 401, "invalid administrator credential")
		return
	}
	if r.Method == "POST" {
		if !takeSlot(w, s.adminSlots) {
			return
		}
		defer func() { <-s.adminSlots }()
	}
	if s.readAdministrator(w, r) {
		return
	}
	var input p.CreateTask
	if r.URL.Path == "/v1/tasks" && r.Method == "POST" {
		if !decode(w, r, &input) {
			return
		}
	}
	response := s.adminResponse(r, input)
	respond(w, response.code, response.value)
}

// All values returned here are immutable snapshots; later changes replace
// records instead of modifying slices/pointers that a response still references.
func (s *Server) adminResponse(r *http.Request, input p.CreateTask) apiReply {
	s.tx.Lock()
	defer s.tx.Unlock()
	var archived p.Task
	if strings.HasPrefix(r.URL.Path, "/v1/tasks/") {
		id := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/tasks/"), "/")[0]
		var err error
		archived, _, err = s.lookupTask(id)
		if err != nil {
			return errorReply(500, "cannot read task")
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case r.URL.Path == "/v1/tasks" && r.Method == "POST":
		input.Command.Defaults()
		if err := input.Command.Validate(); err != nil {
			return errorReply(400, err.Error())
		}
		if !s.nodeExists(input.NodeID) {
			return errorReply(404, "node is not provisioned")
		}
		id, err := p.RandomID()
		if err != nil {
			return errorReply(500, "cannot create task identity")
		}
		t := p.Task{ID: id, NodeID: input.NodeID, Command: input.Command, CreatedAt: time.Now().UTC(), State: "queued"}
		if s.state.Sequences[input.NodeID] == ^uint64(0) {
			return errorReply(503, "task sequence exhausted")
		}
		t.Sequence = s.state.Sequences[input.NodeID] + 1
		if err := s.admit(t); err != nil {
			return errorReply(503, err.Error())
		}
		if err := s.commit(t); err != nil {
			return errorReply(500, "cannot persist task")
		}
		return apiReply{201, t}
	case strings.HasPrefix(r.URL.Path, "/v1/tasks/"):
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/tasks/"), "/")
		t, ok := s.state.Tasks[parts[0]]
		if !ok && archived.ID == parts[0] {
			t, ok = archived, true
		}
		if !ok {
			return errorReply(404, "task not found")
		}
		if len(parts) == 2 && parts[1] == "cancel" && r.Method == "POST" {
			if !p.Terminal(t.State) {
				t.CancelRequested = true
				t.Timing = timingCopy(t)
				if t.Timing.CancelRequestedAt.IsZero() {
					t.Timing.CancelRequestedAt = time.Now().UTC()
				}
				if t.State == "queued" {
					t.State = "canceled"
				}
				if err := s.commit(t); err != nil {
					return errorReply(500, "cannot persist cancellation")
				}
			}
			return apiReply{200, t}
		}
		return errorReply(405, "unsupported task operation")
	default:
		return errorReply(404, "endpoint not found")
	}
}

// commit rolls back memory if the durable replacement fails.
func (s *Server) commit(t p.Task) error {
	// The caller holds tx and mu. Publish a new immutable snapshot only after
	// persistence; releasing mu here keeps status/readers responsive to slow disk.
	next := state{Version: 2, Tasks: make(map[string]p.Task, len(s.state.Tasks)+1), Sequences: copySequences(s.state.Sequences)}
	if t.Sequence > next.Sequences[t.NodeID] {
		next.Sequences[t.NodeID] = t.Sequence
	}
	for id, task := range s.state.Tasks {
		next.Tasks[id] = task
	}
	if completed(t) {
		delete(next.Tasks, t.ID)
	} else {
		next.Tasks[t.ID] = t
	}
	s.mu.Unlock()
	var err error
	if completed(t) {
		err = s.archive(t)
	}
	if err == nil {
		err = s.writeJSON(s.path, next)
	}
	s.mu.Lock()
	if err != nil {
		return err
	}
	s.state = next
	close(s.changed)
	s.changed = make(chan struct{})
	return nil
}

func (s *Server) syncNode(w http.ResponseWriter, r *http.Request, id string) {
	var input p.SyncRequest
	limit := p.MaxBody
	if r.URL.Path == p.NodeControlPath || r.URL.Path == p.NodeRetentionPath {
		limit = 128 * 1024
	}
	if err := p.Decode(http.MaxBytesReader(w, r.Body, int64(limit)), &input); err != nil {
		failure(w, 400, "invalid control request")
		return
	}
	version := p.Version
	if r.URL.Path == p.NodeControlPath {
		version = p.TransferVersion
	}
	if r.URL.Path == p.NodeRetentionPath {
		version = p.LatestVersion
	}
	if input.Version != version {
		failure(w, 400, "unsupported protocol version")
		return
	}
	if !p.ValidID(input.InstanceID) || input.Node.ID != id || !validNodeInfo(input.Node) {
		failure(w, 400, "invalid node identity or capabilities")
		return
	}
	if err := validateReceipts(input, version); err != nil {
		failure(w, 400, err.Error())
		return
	}
	if version >= p.TransferVersion && (input.WaitSeconds < 0 || input.WaitSeconds > p.MaxHeartbeatSeconds*3/2 || input.LeaseSeconds < input.WaitSeconds+30 || input.LeaseSeconds > 604800) {
		failure(w, 400, "invalid heartbeat wait or lease")
		return
	}
	if version >= p.TransferVersion {
		// Bound the response deadline per authenticated long poll; body/header limits remain short.
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(time.Duration(input.WaitSeconds+20) * time.Second))
	}
	seen := time.Now().UTC()
	timer := time.NewTimer(time.Duration(input.WaitSeconds) * time.Second)
	defer timer.Stop()
	for {
		changed := s.syncOnce(w, r, id, input, version, seen, input.WaitSeconds > 0)
		if changed == nil {
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-timer.C:
			s.syncOnce(w, r, id, input, version, seen, false)
			return
		case <-changed:
		}
	}
}

// Return a notification channel only when no immediate control message is needed.
func (s *Server) syncOnce(w http.ResponseWriter, r *http.Request, id string, input p.SyncRequest, version int, seen time.Time, wait bool) <-chan struct{} {
	reply, changed := s.syncState(id, input, version, seen, wait)
	if changed == nil {
		// Waiting may take 45 minutes; sending a response gets only a short deadline.
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(20 * time.Second))
		respond(w, reply.code, reply.value)
	}
	return changed
}

func (s *Server) syncState(id string, input p.SyncRequest, version int, seen time.Time, wait bool) (reply apiReply, changed <-chan struct{}) {
	s.tx.Lock()
	defer s.tx.Unlock()
	var archived p.Task
	if input.Report != nil {
		var err error
		archived, _, err = s.lookupTask(input.Report.TaskID)
		if err != nil {
			return errorReply(500, "cannot read reported task"), nil
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	previous := s.nodes[id]
	if previous.instance != "" && previous.instance != input.InstanceID && previous.info.LeaseExpiresAt != nil && now.Before(*previous.info.LeaseExpiresAt) {
		reply = errorReply(409, "another node instance is active for this node")
		return reply, nil
	}
	input.Node.LastSeen = seen
	lease := 30
	if version >= p.TransferVersion {
		lease = input.LeaseSeconds
	}
	expires := seen.Add(time.Duration(lease) * time.Second)
	input.Node.LeaseExpiresAt = &expires
	if !seen.Before(previous.info.LastSeen) {
		s.nodes[id] = liveNode{info: input.Node, instance: input.InstanceID}
	}
	response := p.SyncResponse{Version: version}
	if version >= p.LatestVersion {
		if err := s.advanceReplay(id, input.ReplayHigh); err != nil {
			return errorReply(500, "cannot persist replay watermark"), nil
		}
		if input.ReplayInit {
			floor := s.state.Sequences[id]
			response.ReplayFloor = &floor
			return apiReply{200, response}, nil
		}
		s.mu.Unlock()
		err := s.releaseReceipts(id, input.Receipts)
		s.mu.Lock()
		if err != nil {
			return errorReply(500, "cannot persist result receipts"), nil
		}
		response.Receipts = input.Receipts
	}
	if report := input.Report; report != nil {
		t, ok := s.state.Tasks[report.TaskID]
		if !ok && archived.ID == report.TaskID {
			t, ok = archived, true
		}
		if !ok || t.NodeID != id {
			reply = errorReply(403, "task does not belong to this node")
			return reply, nil
		}
		if report.State != "running" && !p.Terminal(report.State) {
			reply = errorReply(400, "invalid task state")
			return reply, nil
		}
		if t.State == "queued" {
			reply = errorReply(409, "task has not been dispatched")
			return reply, nil
		}
		if p.Terminal(report.State) && report.Result == nil {
			reply = errorReply(400, "terminal report requires result")
			return reply, nil
		}
		if report.Result != nil && (len(report.Result.Stdout) > t.Command.OutputLimit || len(report.Result.Stderr) > t.Command.OutputLimit) {
			reply = errorReply(400, "result exceeds task output limit")
			return reply, nil
		}
		if version >= p.TransferVersion && p.Terminal(report.State) {
			if report.Upload == nil || report.Upload.Validate(t.Command.OutputLimit) != nil || len(report.Result.Stdout) != 0 || len(report.Result.Stderr) != 0 || len(report.Result.Error) > 8192 || len(report.Result.Executable) > 4096 {
				reply = errorReply(400, "terminal report requires a valid manifest and bounded metadata without output")
				return reply, nil
			}
			if p.Terminal(t.State) && t.Upload != nil && *t.Upload != *report.Upload {
				reply = errorReply(409, "result manifest conflicts with committed report")
				return reply, nil
			}
		}
		// A v1 final acknowledgement may have been lost during a rolling upgrade.
		if version >= p.TransferVersion && p.Terminal(report.State) && p.Terminal(t.State) && t.Upload == nil {
			if t.Result == nil {
				reply = errorReply(409, "terminal task has no result to reconcile")
				return reply, nil
			}
			manifest := p.OutputManifest(t.Result)
			if manifest != *report.Upload {
				reply = errorReply(409, "result conflicts with stored legacy output")
				return reply, nil
			}
			t.Upload = &manifest
			if err := s.commit(t); err != nil {
				reply = errorReply(500, "cannot reconcile legacy result")
				return reply, nil
			}
		}
		if !p.Terminal(t.State) && (t.State != report.State || report.Result != nil) {
			t.State, t.Result = report.State, report.Result
			t.Timing = timingCopy(t)
			if t.State == "running" && t.Timing.RunningReportedAt.IsZero() {
				t.Timing.RunningReportedAt = now
				if !t.Timing.DispatchedAt.IsZero() {
					t.Timing.DispatchToRunningMS = now.Sub(t.Timing.DispatchedAt).Milliseconds()
				}
			}
			if p.Terminal(t.State) {
				t.Timing.TerminalReportedAt = now
				if !t.Timing.CancelRequestedAt.IsZero() {
					t.Timing.CancelToTerminalMS = now.Sub(t.Timing.CancelRequestedAt).Milliseconds()
				}
				if version >= p.TransferVersion {
					t.Upload, t.ResultPending = report.Upload, true
				} else {
					t.Timing.ResultReceivedAt = now
				}
			}
			if err := s.commit(t); err != nil {
				reply = errorReply(500, "cannot persist result")
				return reply, nil
			}
		}
		if p.Terminal(t.State) {
			response.Acknowledged = t.ID
		} else if t.CancelRequested {
			response.CancelTaskID = t.ID
		}
	}
	var selected *p.Task
	for _, t := range s.state.Tasks {
		if t.NodeID != id || p.Terminal(t.State) {
			continue
		}
		candidate := t
		if selected == nil || (candidate.State != "queued" && selected.State == "queued") || (candidate.State == selected.State && (candidate.Sequence < selected.Sequence || (candidate.Sequence == selected.Sequence && candidate.CreatedAt.Before(selected.CreatedAt)))) {
			selected = &candidate
		}
	}
	if selected != nil {
		if selected.State == "queued" && version >= p.TransferVersion && !input.AcceptTask {
			if wait && response.Acknowledged == "" && len(response.Receipts) == 0 {
				return reply, s.changed
			}
			reply = apiReply{200, response}
			return reply, nil
		}
		if selected.State == "queued" {
			selected.State = "dispatched"
			selected.Timing = timingCopy(*selected)
			selected.Timing.DispatchedAt = now
			selected.Timing.QueueMS = now.Sub(selected.CreatedAt).Milliseconds()
			if err := s.commit(*selected); err != nil {
				reply = errorReply(500, "cannot persist dispatch")
				return reply, nil
			}
		}
		if version == p.Version {
			selected.Timing, selected.Upload, selected.ResultPending = nil, nil, false
		}
		if version < p.LatestVersion {
			selected.Sequence = 0
		}
		response.Task = selected
		if selected.CancelRequested {
			response.CancelTaskID = selected.ID
		}
	}
	if wait && response.Acknowledged == "" && response.CancelTaskID == "" && len(response.Receipts) == 0 {
		if selected == nil || (input.Report != nil && input.Report.TaskID == selected.ID && input.Report.State == "running" && selected.State == "running") {
			return reply, s.changed
		}
	}
	reply = apiReply{200, response}
	return reply, nil
}

func (s *Server) String() string {
	return fmt.Sprintf("tack-control (%d provisioned nodes)", len(s.config.Nodes))
}

// Copy before mutation so failed commits leave the previous state intact.
func timingCopy(t p.Task) *p.TaskTiming {
	v := p.TaskTiming{}
	if t.Timing != nil {
		v = *t.Timing
	}
	return &v
}

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(code int) { w.code = code; w.ResponseWriter.WriteHeader(code) }

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

type apiReply struct {
	code  int
	value any
}

func errorReply(code int, message string) apiReply {
	return apiReply{code, map[string]string{"error": message}}
}
