package control

import (
	"encoding/json"
	"errors"
	"net/http"
	p "tack.local/management/protocol"
	"time"
)

type taskFilter struct {
	principal                  principal
	node, state, caller, owner string
	since, until               time.Time
}

func parseFilter(r *http.Request) (taskFilter, error) {
	q := r.URL.Query()
	f := taskFilter{principal: principalFor(r), node: q.Get("node"), state: q.Get("state"), caller: q.Get("caller_id"), owner: q.Get("owner")}
	for _, id := range []string{f.node, f.caller, f.owner} {
		if id != "" && !p.ValidID(id) {
			return f, errors.New("invalid task filter identity")
		}
	}
	if f.state != "" && f.state != "queued" && f.state != "dispatched" && f.state != "running" && !p.Terminal(f.state) {
		return f, errors.New("invalid task state filter")
	}
	for key, dest := range map[string]*time.Time{"since": &f.since, "until": &f.until} {
		if q.Get(key) != "" {
			t, err := time.Parse(time.RFC3339Nano, q.Get(key))
			if err != nil {
				return f, errors.New("time filters must use RFC3339")
			}
			*dest = t
		}
	}
	if !f.since.IsZero() && !f.until.IsZero() && f.since.After(f.until) {
		return f, errors.New("since must not be after until")
	}
	return f, nil
}
func (f taskFilter) enabled() bool {
	return !f.principal.admin || f.node != "" || f.state != "" || f.caller != "" || f.owner != "" || !f.since.IsZero() || !f.until.IsZero()
}
func (f taskFilter) matches(t p.Task) bool {
	return f.principal.allows(t.NodeID) && (f.node == "" || f.node == t.NodeID) && (f.state == "" || f.state == t.State) && (f.caller == "" || f.caller == t.CallerID) && (f.owner == "" || f.owner == t.Owner) && (f.since.IsZero() || !t.CreatedAt.Before(f.since)) && (f.until.IsZero() || !t.CreatedAt.After(f.until))
}

// Skip command/output allocation while inspecting an archive for list filters.
func summaryMetadata(data []byte) (p.Task, error) {
	var m struct {
		ID        string    `json:"id"`
		NodeID    string    `json:"node_id"`
		State     string    `json:"state"`
		CallerID  string    `json:"caller_id"`
		Owner     string    `json:"owner"`
		CreatedAt time.Time `json:"created_at"`
	}
	err := json.Unmarshal(data, &m)
	return p.Task{ID: m.ID, NodeID: m.NodeID, State: m.State, CallerID: m.CallerID, Owner: m.Owner, CreatedAt: m.CreatedAt}, err
}

// Called with mu held. Delivery problems are observations, not new execution
// outcomes. Once the result is complete a stale upload warning is suppressed.
func (s *Server) deliveryStatus(t p.Task) p.Task {
	if completed(t) {
		return t
	}
	if n := s.nodes[t.NodeID].info.Status; n != nil && n.Transfer != nil && n.Transfer.TaskID == t.ID {
		t.Transfer = n.Transfer
	}
	return t
}
