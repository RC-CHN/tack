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
	"sort"
	"strings"
	"sync"
	"time"

	p "tack.local/management/protocol"
)

type state struct {
	Tasks map[string]p.Task `json:"tasks"`
}
type liveNode struct {
	info     p.Node
	instance string
}

// Server is a single-controller durable task queue. Run one process per state file.
type Server struct {
	mu     sync.Mutex
	config p.ControlConfig
	path   string
	state  state
	nodes  map[string]liveNode
}

func ValidateConfig(c p.ControlConfig) error {
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
	s := &Server{config: config, path: path, state: state{Tasks: map[string]p.Task{}}, nodes: map[string]liveNode{}}
	if err := p.ReadJSON(path, &s.state); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if s.state.Tasks == nil {
		s.state.Tasks = map[string]p.Task{}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	if err := p.WriteJSON(path, s.state); err != nil {
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
		if r.URL.Path != p.NodeSyncPath && r.URL.Path != "/health" {
			failure(w, 404, "endpoint not found")
			return
		}
		s.ServeHTTP(w, r)
	})
}

func (s *Server) AdminHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == p.NodeSyncPath {
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

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/health" && r.Method == "GET" {
		respond(w, 200, map[string]int{"protocol_version": p.Version})
		return
	}
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, "Bearer ") {
		failure(w, 401, "authentication required")
		return
	}
	token := strings.TrimPrefix(header, "Bearer ")
	if r.URL.Path == p.NodeSyncPath {
		if r.Method != "POST" {
			failure(w, 405, "POST required")
			return
		}
		for _, n := range s.config.Nodes {
			if equalToken(token, n.Token) {
				s.syncNode(w, r, n.ID)
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
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case r.URL.Path == "/v1/nodes" && r.Method == "GET":
		nodes := make([]p.Node, 0, len(s.config.Nodes))
		for _, n := range s.config.Nodes {
			v := s.nodes[n.ID].info
			v.ID = n.ID
			nodes = append(nodes, v)
		}
		sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
		respond(w, 200, nodes)
	case r.URL.Path == "/v1/tasks" && r.Method == "POST":
		var input p.CreateTask
		if !decode(w, r, &input) {
			return
		}
		input.Command.Defaults()
		if err := input.Command.Validate(); err != nil {
			failure(w, 400, err.Error())
			return
		}
		if !s.nodeExists(input.NodeID) {
			failure(w, 404, "node is not provisioned")
			return
		}
		id, err := p.RandomID()
		if err != nil {
			failure(w, 500, "cannot create task identity")
			return
		}
		t := p.Task{ID: id, NodeID: input.NodeID, Command: input.Command, CreatedAt: time.Now().UTC(), State: "queued"}
		if err := s.commit(t); err != nil {
			failure(w, 500, "cannot persist task")
			return
		}
		respond(w, 201, t)
	case r.URL.Path == "/v1/tasks" && r.Method == "GET":
		tasks := make([]p.Task, 0, len(s.state.Tasks))
		for _, t := range s.state.Tasks {
			tasks = append(tasks, t)
		}
		sort.Slice(tasks, func(i, j int) bool { return tasks[i].CreatedAt.Before(tasks[j].CreatedAt) })
		respond(w, 200, tasks)
	case strings.HasPrefix(r.URL.Path, "/v1/tasks/"):
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/tasks/"), "/")
		t, ok := s.state.Tasks[parts[0]]
		if !ok {
			failure(w, 404, "task not found")
			return
		}
		if len(parts) == 1 && r.Method == "GET" {
			respond(w, 200, t)
			return
		}
		if len(parts) == 2 && parts[1] == "cancel" && r.Method == "POST" {
			if !p.Terminal(t.State) {
				t.CancelRequested = true
				if t.State == "queued" {
					t.State = "canceled"
				}
				if err := s.commit(t); err != nil {
					failure(w, 500, "cannot persist cancellation")
					return
				}
			}
			respond(w, 200, t)
			return
		}
		failure(w, 405, "unsupported task operation")
	default:
		failure(w, 404, "endpoint not found")
	}
}

// commit rolls back memory if the durable replacement fails.
func (s *Server) commit(t p.Task) error {
	old, exists := s.state.Tasks[t.ID]
	s.state.Tasks[t.ID] = t
	if err := p.WriteJSON(s.path, s.state); err != nil {
		if exists {
			s.state.Tasks[t.ID] = old
		} else {
			delete(s.state.Tasks, t.ID)
		}
		return err
	}
	return nil
}

func (s *Server) syncNode(w http.ResponseWriter, r *http.Request, id string) {
	var input p.SyncRequest
	if !decode(w, r, &input) {
		return
	}
	if input.Version != p.Version {
		failure(w, 400, "unsupported protocol version")
		return
	}
	if !p.ValidID(input.InstanceID) || input.Node.ID != id || len(input.Node.Hostname) > 255 || len(input.Node.Modes) > 16 {
		failure(w, 400, "invalid node identity or capabilities")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	previous := s.nodes[id]
	if previous.instance != "" && previous.instance != input.InstanceID && now.Sub(previous.info.LastSeen) < 30*time.Second {
		failure(w, 409, "another node instance is active for this node")
		return
	}
	input.Node.LastSeen = now
	s.nodes[id] = liveNode{info: input.Node, instance: input.InstanceID}
	response := p.SyncResponse{Version: p.Version}
	if report := input.Report; report != nil {
		t, ok := s.state.Tasks[report.TaskID]
		if !ok || t.NodeID != id {
			failure(w, 403, "task does not belong to this node")
			return
		}
		if report.State != "running" && !p.Terminal(report.State) {
			failure(w, 400, "invalid task state")
			return
		}
		if t.State == "queued" {
			failure(w, 409, "task has not been dispatched")
			return
		}
		if p.Terminal(report.State) && report.Result == nil {
			failure(w, 400, "terminal report requires result")
			return
		}
		if report.Result != nil && (len(report.Result.Stdout) > t.Command.OutputLimit || len(report.Result.Stderr) > t.Command.OutputLimit) {
			failure(w, 400, "result exceeds task output limit")
			return
		}
		if !p.Terminal(t.State) && (t.State != report.State || report.Result != nil) {
			t.State, t.Result = report.State, report.Result
			if err := s.commit(t); err != nil {
				failure(w, 500, "cannot persist result")
				return
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
		if selected == nil || (candidate.State != "queued" && selected.State == "queued") || (candidate.State == selected.State && candidate.CreatedAt.Before(selected.CreatedAt)) {
			selected = &candidate
		}
	}
	if selected != nil {
		if selected.State == "queued" {
			selected.State = "dispatched"
			if err := s.commit(*selected); err != nil {
				failure(w, 500, "cannot persist dispatch")
				return
			}
		}
		response.Task = selected
		if selected.CancelRequested {
			response.CancelTaskID = selected.ID
		}
	}
	respond(w, 200, response)
}

func (s *Server) String() string {
	return fmt.Sprintf("tack-control (%d provisioned nodes)", len(s.config.Nodes))
}
