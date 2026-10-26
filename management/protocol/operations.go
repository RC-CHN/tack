package protocol

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Version 4 adds deadline enforcement and operation diagnostics. Version 3
// remains the retention protocol for rolling upgrades.
const OperationsVersion = 4
const NodeOperationsPath = "/v4/node/sync"

type CleanupResult struct {
	ErrorCode string `json:"error_code,omitempty"`
	Error     string `json:"error,omitempty"`
	Path      string `json:"path,omitempty"`
}

type Problem struct {
	ErrorCode string `json:"error_code"`
	Phase     string `json:"phase"`
	Error     string `json:"error"`
}

type TransferStatus struct {
	TaskID    string    `json:"task_id"`
	ErrorCode string    `json:"error_code,omitempty"`
	Phase     string    `json:"phase"`
	Error     string    `json:"error,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

type TunnelStatus struct {
	State     string    `json:"state"` // disabled, connecting, connected, disconnected, unknown
	UpdatedAt time.Time `json:"updated_at"`
}

type NodeStatus struct {
	Management     string          `json:"management"` // connected, lease_valid, offline, never_seen
	AcceptingTasks bool            `json:"accepting_tasks"`
	CurrentTask    string          `json:"current_task,omitempty"`
	BacklogReason  string          `json:"backlog_reason,omitempty"`
	UpdatedAt      time.Time       `json:"updated_at"`
	Tunnel         TunnelStatus    `json:"tunnel"`
	Transfer       *TransferStatus `json:"transfer,omitempty"`
}

// Timestamped identities allow a durable retirement watermark instead of an
// unbounded deduplication ledger. Never replace an identity when retrying.
func NewRequestID() (string, error) {
	id, err := RandomID()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%013d-%s", time.Now().UnixMilli(), id), nil
}

func RequestTime(id string) (int64, error) {
	parts := strings.SplitN(id, "-", 2)
	if len(parts) != 2 || len(parts[0]) != 13 || len(parts[1]) < 16 || len(parts[1]) > 64 || !ValidID(parts[1]) {
		return 0, errors.New("request_id must be <13-digit Unix milliseconds>-<16..64 character unique ID>; use tack request-id")
	}
	n, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || n <= 0 || fmt.Sprintf("%013d", n) != parts[0] {
		return 0, errors.New("invalid request_id timestamp")
	}
	return n, nil
}

func DeadlineExpired(deadline *time.Time, now time.Time) bool {
	return deadline != nil && !now.Before(*deadline)
}

func ExpiredResult() *Result {
	return &Result{FinishedAt: time.Now().UTC(), ErrorCode: "start_deadline_exceeded", Phase: "queued", Error: "latest allowed start time has passed; command was not started"}
}

// Operators are stored by the controller, separately from node credentials.
// Tokens are supplied only on creation/rotation and never returned by listing.
type Operator struct {
	ID       string   `json:"id"`
	Token    string   `json:"token,omitempty"`
	NodeIDs  []string `json:"node_ids"`
	Read     bool     `json:"read"`
	Execute  bool     `json:"execute"`
	Disabled bool     `json:"disabled,omitempty"`
}

type ClientCredential struct {
	Token string `json:"token"`
}
