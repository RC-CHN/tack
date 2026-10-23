package control

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	p "tack.local/management/protocol"
)

const maxTaskFile = 16 * 1024 * 1024

func storageDefaults(c *p.ControlConfig) error {
	if c.MaxConcurrentUploads == 0 {
		c.MaxConcurrentUploads = 8
	}
	if c.MaxConcurrentUploads < 1 || c.MaxConcurrentUploads > 64 {
		return errors.New("max_concurrent_uploads must be 1..64")
	}
	if c.MaxActiveTasks == 0 {
		c.MaxActiveTasks = 256
	}
	if c.MaxActiveBytes == 0 {
		c.MaxActiveBytes = 16 * 1024 * 1024
	}
	if c.MaxHistoryTasks == 0 {
		c.MaxHistoryTasks = 100000
	}
	if c.MaxHistoryBytes == 0 {
		c.MaxHistoryBytes = 1024 * 1024 * 1024
	}
	if c.HistoryRetentionSeconds == 0 {
		c.HistoryRetentionSeconds = 30 * 86400
	}
	if c.HistoryRetentionSeconds < 1 || c.HistoryRetentionSeconds > 10*365*86400 {
		return errors.New("history_retention_seconds must be 1..315360000")
	}
	if c.MaxActiveTasks < 1 || c.MaxActiveTasks > 10000 || c.MaxActiveBytes < p.MaxBody || c.MaxActiveBytes > 1024*1024*1024 || c.MaxHistoryTasks < 1 || c.MaxHistoryTasks > 10000000 || c.MaxHistoryBytes < maxTaskFile {
		return errors.New("invalid controller storage limits")
	}
	return nil
}

func completed(t p.Task) bool                  { return p.Terminal(t.State) && !t.ResultPending }
func (s *Server) historyPath(id string) string { return filepath.Join(s.path+".history", id+".json") }
func (s *Server) releasedPath(id string) string {
	return filepath.Join(s.path+".history", "released", id+".json")
}

// Active work is bounded independently of completed history. Capacity is
// reserved before acceptance so draining accepted results remains possible.
func (s *Server) admit(t p.Task) error {
	// tx stays held; read-only APIs must not wait on a directory scan or deletion.
	s.mu.Unlock()
	err := s.pruneHistory(&t)
	s.mu.Lock()
	if err != nil {
		return err
	}
	if len(s.state.Tasks) >= s.config.MaxActiveTasks || s.historyTasks+s.orphanFiles+len(s.state.Tasks) >= s.config.MaxHistoryTasks {
		return errors.New("active or unconfirmed task capacity reached; confirmed history has been reclaimed")
	}
	var active, reserved int64
	for _, task := range s.state.Tasks {
		b, _ := json.Marshal(task.Command)
		active += int64(len(b)) + 32768
		reserved += reservation(task)
	}
	b, _ := json.Marshal(t.Command)
	if active+int64(len(b))+32768 > s.config.MaxActiveBytes || s.historyBytes+s.orphanBytes+reserved+reservation(t) > s.config.MaxHistoryBytes {
		return errors.New("active or unconfirmed byte capacity reached; confirmed history has been reclaimed")
	}
	return nil
}

func reservation(t p.Task) int64 {
	b, _ := json.MarshalIndent(t.Command, "", "  ")
	return int64(len(b)) + 6*int64(t.Command.OutputLimit) + 64*1024
}

// archive is called under tx (or during startup), never concurrently with
// another durable mutation. Historical payloads are never cached in memory.
func (s *Server) archive(t p.Task) error {
	path := s.historyPath(t.ID)
	if t.State == "canceled" && (t.Timing == nil || t.Timing.DispatchedAt.IsZero()) {
		path = s.releasedPath(t.ID)
		// rc.1 stored never-dispatched cancellations in the root directory.
		// Move an existing archive before rewriting it so migration cannot
		// leave duplicate files or double-count its byte/count reservation.
		if _, err := os.Stat(s.historyPath(t.ID)); err == nil {
			if err := s.moveReleased(t.ID); err != nil {
				return err
			}
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	if _, err := os.Stat(s.releasedPath(t.ID)); err == nil {
		path = s.releasedPath(t.ID)
	}
	var old int64
	exists := false
	if info, err := os.Stat(path); err == nil {
		old, exists = info.Size(), true
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := s.writeJSON(path, t); err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if path == s.releasedPath(t.ID) {
		if exists {
			s.evictions.valid = false
		} else if s.evictions.valid {
			s.evictions.add(eviction{id: t.ID, size: info.Size(), modified: info.ModTime()})
		}
	}
	s.historyBytes += info.Size() - old
	if !exists {
		s.historyTasks++
	}
	return nil
}

// Stream directory entries rather than allocating an entry for all history.
func walkFiles(dir string, fn func(os.DirEntry) error) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	for {
		entries, err := f.ReadDir(128)
		for _, entry := range entries {
			if e := fn(entry); e != nil {
				return e
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func (s *Server) openStorage() error {
	dir := s.path + ".history"
	if err := os.MkdirAll(filepath.Join(dir, "released"), 0700); err != nil {
		return err
	}
	for _, d := range []string{dir, filepath.Join(dir, "released")} {
		if err := cleanHistoryTemps(d); err != nil {
			return err
		}
	}
	if err := p.SyncDirectory(filepath.Dir(dir)); err != nil {
		return err
	}
	if err := s.walkHistory(func(_ string, e os.DirEntry) error {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			return nil
		}
		if !p.ValidID(strings.TrimSuffix(e.Name(), ".json")) {
			return fmt.Errorf("invalid history entry %q", e.Name())
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Size() > maxTaskFile {
			return fmt.Errorf("invalid history file %q", e.Name())
		}
		s.historyTasks++
		s.historyBytes += info.Size()
		return nil
	}); err != nil {
		return err
	}
	f, err := os.Open(s.path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	// Migrate legacy snapshots one task at a time. The source is replaced only
	// after every archive has been made durable, so interrupted migration retries.
	d := json.NewDecoder(f)
	d.DisallowUnknownFields()
	if token, err := d.Token(); err != nil || token != json.Delim('{') {
		return errors.New("invalid controller state")
	}
	seen := false
	versionSeen := false
	storageVersion := 0
	sequencesSeen := false
	var activeBytes int64
	for d.More() {
		key, err := d.Token()
		if err == nil && key == "sequences" && !sequencesSeen {
			if err := d.Decode(&s.state.Sequences); err != nil {
				return err
			}
			if s.state.Sequences == nil {
				return errors.New("invalid sequence state")
			}
			for id := range s.state.Sequences {
				if !p.ValidID(id) {
					return errors.New("invalid sequence node")
				}
			}
			sequencesSeen = true
			continue
		}
		if err == nil && key == "storage_version" && !versionSeen {
			var version int
			if err := d.Decode(&version); err != nil || (version != 1 && version != 2) {
				return errors.New("unsupported storage version")
			}
			versionSeen = true
			storageVersion = version
			continue
		}
		if err != nil || key != "tasks" || seen {
			return errors.New("invalid controller state fields")
		}
		seen = true
		token, err := d.Token()
		if err != nil {
			return err
		}
		if token == nil {
			continue
		}
		if token != json.Delim('{') {
			return errors.New("invalid task map")
		}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return err
			}
			id, ok := key.(string)
			if !ok || !p.ValidID(id) {
				return errors.New("invalid stored task ID")
			}
			var task p.Task
			if err := d.Decode(&task); err != nil {
				return err
			}
			if task.ID != id || !p.ValidID(task.NodeID) {
				return errors.New("stored task identity mismatch")
			}
			if completed(task) {
				if err := s.archive(task); err != nil {
					return err
				}
				continue
			}
			// An archive may have committed just before a crash replacing the
			// active snapshot, or survive restoration of an older snapshot.
			// Its durable completion wins; never resurrect an already done task.
			if saved, ok, err := s.readHistory(id); err != nil {
				return err
			} else if ok {
				if saved.NodeID != task.NodeID {
					return errors.New("history node does not match active snapshot")
				}
				continue
			}
			b, _ := json.Marshal(task)
			activeBytes += int64(len(b))
			if len(s.state.Tasks) >= s.config.MaxActiveTasks || activeBytes > s.config.MaxActiveBytes {
				return errors.New("existing active queue exceeds limits; increase configured limits before migration")
			}
			s.state.Tasks[id] = task
		}
		if _, err := d.Token(); err != nil {
			return err
		}
	}
	if _, err := d.Token(); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("unexpected trailing state data")
	}
	if storageVersion < 2 {
		return s.releaseLegacyCancellations()
	}
	return nil
}

func (s *Server) lookupTask(id string) (p.Task, bool, error) {
	if !p.ValidID(id) {
		return p.Task{}, false, nil
	}
	s.mu.Lock()
	t, ok := s.state.Tasks[id]
	s.mu.Unlock()
	if ok {
		return t, true, nil
	}
	return s.readHistory(id)
}

func (s *Server) readHistory(id string) (p.Task, bool, error) {
	var t p.Task
	f, err := os.Open(s.historyPath(id))
	if os.IsNotExist(err) {
		f, err = os.Open(s.releasedPath(id))
	}
	if os.IsNotExist(err) {
		return t, false, nil
	}
	if err != nil {
		return t, false, err
	}
	defer f.Close()
	if err := p.Decode(io.LimitReader(f, maxTaskFile+1), &t); err != nil {
		return t, false, err
	}
	if t.ID != id || !completed(t) {
		return t, false, errors.New("invalid historical task")
	}
	return t, true, nil
}

func taskSummary(t p.Task) p.Task {
	t.Command = p.Command{}
	t.Result = nil
	return t
}

// Pagination is ordered by opaque task ID, not submission time. The directory
// scan uses constant memory; only one bounded page of task summaries is read.
func (s *Server) taskPage(after string, limit int) (p.TaskPage, error) {
	s.mu.Lock()
	active := make(map[string]p.Task, len(s.state.Tasks))
	for id, t := range s.state.Tasks {
		active[id] = taskSummary(t)
	}
	s.mu.Unlock()
	ids := make([]string, 0, limit+2)
	add := func(id string) {
		if id <= after {
			return
		}
		index := sort.SearchStrings(ids, id)
		if index < len(ids) && ids[index] == id {
			return
		}
		if index > limit {
			return
		}
		ids = append(ids, "")
		copy(ids[index+1:], ids[index:])
		ids[index] = id
		if len(ids) > limit+1 {
			ids = ids[:limit+1]
		}
	}
	for id := range active {
		add(id)
	}
	if err := s.walkHistory(func(_ string, e os.DirEntry) error {
		id := strings.TrimSuffix(e.Name(), ".json")
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") && p.ValidID(id) {
			add(id)
		}
		return nil
	}); err != nil {
		return p.TaskPage{}, err
	}
	page := p.TaskPage{Tasks: make([]p.Task, 0, limit)}
	if len(ids) > limit {
		page.NextCursor = ids[limit-1]
		ids = ids[:limit]
	}
	for _, id := range ids {
		t, ok := active[id]
		if !ok {
			var err error
			t, ok, err = s.lookupTask(id)
			if err != nil {
				return page, err
			}
			if !ok {
				continue
			}
		}
		page.Tasks = append(page.Tasks, taskSummary(t))
	}
	return page, nil
}

func (s *Server) readAdministrator(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != "GET" {
		return false
	}
	if r.URL.Path == "/v1/tasks" || r.URL.Path == "/v2/tasks" || strings.HasPrefix(r.URL.Path, "/v1/tasks/") {
		if !takeSlot(w, s.historySlots) {
			return true
		}
		defer func() { <-s.historySlots }()
	}
	switch {
	case r.URL.Path == "/v1/nodes" || r.URL.Path == "/v1/metrics":
		s.mu.Lock()
		nodes := make([]p.Node, 0, len(s.config.Nodes))
		for _, n := range s.config.Nodes {
			v := s.nodes[n.ID].info
			v.ID = n.ID
			v.Online = v.LeaseExpiresAt != nil && time.Now().Before(*v.LeaseExpiresAt)
			nodes = append(nodes, v)
		}
		s.mu.Unlock()
		sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
		if r.URL.Path == "/v1/metrics" {
			respond(w, 200, p.MetricsSnapshot{Controller: s.meter.Snapshot(), Nodes: nodes})
		} else {
			respond(w, 200, nodes)
		}
	case r.URL.Path == "/v1/tasks" || r.URL.Path == "/v2/tasks":
		limit := 100
		if value := r.URL.Query().Get("limit"); value != "" {
			n, err := strconv.Atoi(value)
			if err != nil || n < 1 || n > 200 {
				failure(w, 400, "limit must be 1..200")
				return true
			}
			limit = n
		}
		after := r.URL.Query().Get("after")
		if after != "" && !p.ValidID(after) {
			failure(w, 400, "invalid cursor")
			return true
		}
		page, err := s.taskPage(after, limit)
		if err != nil {
			failure(w, 500, "cannot read task history")
			return true
		}
		if r.URL.Path == "/v1/tasks" {
			w.Header().Set("X-Next-Cursor", page.NextCursor)
			respond(w, 200, page.Tasks)
		} else {
			respond(w, 200, page)
		}
	case strings.HasPrefix(r.URL.Path, "/v1/tasks/") && !strings.Contains(strings.TrimPrefix(r.URL.Path, "/v1/tasks/"), "/"):
		t, ok, err := s.lookupTask(strings.TrimPrefix(r.URL.Path, "/v1/tasks/"))
		if err != nil {
			failure(w, 500, "cannot read task")
		} else if !ok {
			failure(w, 404, "task not found")
		} else {
			respond(w, 200, t)
		}
	default:
		return false
	}
	return true
}

// Only files provably belonging to completed uploads are removed. Pending
// uploads survive restart and are still recoverable at the acknowledged offset.
func (s *Server) cleanUploads() error {
	dir := s.path + ".uploads"
	s.orphanBytes, s.orphanFiles = 0, 0
	preserve := func(e os.DirEntry) error {
		info, err := e.Info()
		if err != nil {
			return err
		}
		s.orphanFiles++
		size := info.Size()
		if size < 4096 {
			size = 4096
		}
		s.orphanBytes += size
		return nil
	}
	err := walkFiles(dir, func(e os.DirEntry) error {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".part") {
			return preserve(e)
		}
		name := strings.TrimSuffix(e.Name(), ".part")
		cut := strings.LastIndexByte(name, '.')
		if cut < 1 {
			return preserve(e)
		}
		id, digest := name[:cut], name[cut+1:]
		t, ok, err := s.lookupTask(id)
		if err != nil {
			return err
		}
		if !ok || t.Upload == nil || t.Upload.SHA256 != digest {
			return preserve(e)
		}
		if !completed(t) {
			return nil
		}
		return os.Remove(filepath.Join(dir, e.Name()))
	})
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return p.SyncDirectory(dir)
}
