package control

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	p "tack.local/management/protocol"
)

func requestTaskID(owner, request string) string {
	h := sha256.Sum256([]byte(owner + "\x00" + request))
	return "r" + strings.SplitN(request, "-", 2)[0] + "-" + hex.EncodeToString(h[:20])
}

// Called under tx, before mu. The task itself is the durable idempotency record;
// there is no second write that could acknowledge an uncommitted submission.
func (s *Server) repeatedRequest(r *http.Request, in p.CreateTask) (apiReply, bool) {
	if in.RequestID == "" {
		return apiReply{}, false
	} // legacy API clients
	stamp, err := p.RequestTime(in.RequestID)
	if err != nil {
		return problemReply(400, "invalid_request_id", "submit", err.Error()), true
	}
	id := requestTaskID(principalFor(r).id, in.RequestID)
	t, ok, err := s.lookupTask(id)
	if err != nil {
		return problemReply(500, "storage_read_failed", "submit", "cannot read idempotency record"), true
	}
	if ok {
		original := p.CreateTask{NodeID: t.NodeID, Command: t.Command, RequestID: t.RequestID, CallerID: t.CallerID, StartBefore: t.StartBefore}
		a, _ := json.Marshal(original)
		b, _ := json.Marshal(in)
		if string(a) != string(b) {
			return problemReply(409, "request_conflict", "submit", "request_id was already used with different parameters"), true
		}
		return apiReply{200, t}, true
	}
	if stamp <= s.state.RequestFloor || stamp < time.Now().Add(-7*24*time.Hour).UnixMilli() {
		return problemReply(410, "request_retired", "submit", "request identity is retired; verify the original outcome; do not resubmit with a new identity"), true
	}
	if stamp > time.Now().Add(5*time.Minute).UnixMilli() {
		return problemReply(400, "request_clock_skew", "submit", "request_id is more than five minutes in the future"), true
	}
	return apiReply{}, false
}

// Persist a conservative watermark BEFORE unlinking a deduplicated task. A
// crash can retire some unused older IDs but cannot allow an evicted ID to run
// twice. Memory and disk use remain constant, independent of task count.
// tx held; mu must not be held by the caller.
func (s *Server) retireRequest(taskID string) error {
	if !strings.HasPrefix(taskID, "r") {
		return nil
	}
	stamp, err := p.RequestTime(strings.TrimPrefix(taskID, "r"))
	if err != nil {
		return nil
	} // pre-feature random task identity
	s.mu.Lock()
	next := s.state
	s.mu.Unlock()
	if stamp <= next.RequestFloor {
		return nil
	}
	next.RequestFloor = stamp
	if err := s.writeJSON(s.path, next); err != nil {
		return err
	}
	s.mu.Lock()
	s.state = next
	s.mu.Unlock()
	return nil
}

func problemReply(status int, code, phase, message string) apiReply {
	return apiReply{status, p.Problem{ErrorCode: code, Phase: phase, Error: message}}
}

func (s *Server) expireQueued(now time.Time) error {
	// tx is held, mu is not. Expiration never guesses about a dispatched task.
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.state.Tasks {
		if t.State == "queued" && p.DeadlineExpired(t.StartBefore, now) {
			t.State, t.Result = "expired", p.ExpiredResult()
			if err := s.commit(t); err != nil {
				return err
			}
		}
	}
	return nil
}
