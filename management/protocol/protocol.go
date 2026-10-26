// Package protocol defines the versioned management wire format.
package protocol

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const Version = 1
const TransferVersion = 2
const RetentionVersion = 3
const LatestVersion = OperationsVersion

// NodeSyncPath retains the version 1 wire endpoint across the Tack rename.
const NodeSyncPath = "/v1/agent/sync"
const NodeControlPath = "/v2/node/sync"
const NodeRetentionPath = "/v3/node/sync"
const NodeResultPath = "/v2/node/results"
const MaxChunk = 64 * 1024
const ResultTimeoutSeconds = 120

const MaxOutput = 1024 * 1024 // per output stream
const MaxBody = 4 * 1024 * 1024

var identifier = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,95}$`)

func ValidID(s string) bool { return identifier.MatchString(s) }

type Command struct {
	Mode           string            `json:"mode"`
	Program        string            `json:"program,omitempty"`
	Args           []string          `json:"args,omitempty"`
	Script         string            `json:"script,omitempty"`
	Dir            string            `json:"dir,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	TimeoutSeconds int               `json:"timeout_seconds"`
	OutputLimit    int               `json:"output_limit"`
}

func (c *Command) Defaults() {
	if c.TimeoutSeconds == 0 {
		c.TimeoutSeconds = 60
	}
	if c.OutputLimit == 0 {
		c.OutputLimit = 65536
	}
}

func (c Command) Validate() error {
	if c.TimeoutSeconds < 1 || c.TimeoutSeconds > 86400 {
		return errors.New("timeout_seconds must be 1..86400")
	}
	if c.OutputLimit < 1 || c.OutputLimit > MaxOutput {
		return errors.New("output_limit must be 1..1048576")
	}
	if strings.ContainsRune(c.Dir, 0) {
		return errors.New("invalid directory")
	}
	if len(c.Script) > 256*1024 || len(c.Args) > 256 || len(c.Env) > 128 {
		return errors.New("command too large")
	}
	for _, arg := range c.Args {
		if strings.ContainsRune(arg, 0) || len(arg) > 32768 {
			return errors.New("invalid argument")
		}
	}
	for k, v := range c.Env {
		if k == "" || strings.ContainsAny(k, "=\x00") || strings.ContainsRune(v, 0) || len(v) > 32768 {
			return errors.New("invalid environment")
		}
	}
	switch c.Mode {
	case "exec":
		if c.Program == "" || strings.ContainsRune(c.Program, 0) || c.Script != "" {
			return errors.New("exec requires program and no script")
		}
	case "posix-sh", "portable-sh", "bash", "zsh", "powershell", "cmd":
		if c.Script == "" || c.Program != "" || len(c.Args) != 0 || strings.ContainsRune(c.Script, 0) {
			return errors.New("script mode requires script and no program/args")
		}
	default:
		return fmt.Errorf("unsupported mode %q", c.Mode)
	}
	return nil
}

type Task struct {
	ID              string          `json:"id"`
	Sequence        uint64          `json:"sequence,omitempty"`
	NodeID          string          `json:"node_id"`
	RequestID       string          `json:"request_id,omitempty"`
	CallerID        string          `json:"caller_id,omitempty"`
	Owner           string          `json:"owner,omitempty"`
	StartBefore     *time.Time      `json:"start_before,omitempty"`
	Command         Command         `json:"command"`
	CreatedAt       time.Time       `json:"created_at"`
	State           string          `json:"state"`
	CancelRequested bool            `json:"cancel_requested"`
	Result          *Result         `json:"result,omitempty"`
	ResultPending   bool            `json:"result_pending,omitempty"`
	Upload          *Manifest       `json:"upload,omitempty"`
	Timing          *TaskTiming     `json:"timing,omitempty"`
	Transfer        *TransferStatus `json:"transfer,omitempty"`
}

type Result struct {
	ErrorCode         string         `json:"error_code,omitempty"`
	Phase             string         `json:"phase,omitempty"`
	Cleanup           *CleanupResult `json:"cleanup,omitempty"`
	MetadataTruncated bool           `json:"metadata_truncated,omitempty"`
	StartedAt         time.Time      `json:"started_at"`
	FinishedAt        time.Time      `json:"finished_at"`
	Executable        string         `json:"executable,omitempty"`
	ExitCode          *int           `json:"exit_code,omitempty"`
	Stdout            []byte         `json:"stdout,omitempty"`
	Stderr            []byte         `json:"stderr,omitempty"`
	StdoutTruncated   bool           `json:"stdout_truncated"`
	StderrTruncated   bool           `json:"stderr_truncated"`
	Error             string         `json:"error,omitempty"`
}

func Terminal(state string) bool {
	switch state {
	case "succeeded", "failed", "timed_out", "canceled", "unknown", "expired":
		return true
	}
	return false
}

type Node struct {
	Status           *NodeStatus  `json:"status,omitempty"`
	ID               string       `json:"id"`
	Hostname         string       `json:"hostname"`
	OS               string       `json:"os"`
	Arch             string       `json:"arch"`
	Modes            []string     `json:"modes"`
	StatePersistence string       `json:"state_persistence"`
	LastSeen         time.Time    `json:"last_seen"`
	LeaseExpiresAt   *time.Time   `json:"lease_expires_at,omitempty"`
	Online           bool         `json:"online,omitempty"`
	Metrics          *NodeMetrics `json:"metrics,omitempty"`
}

type Report struct {
	TaskID string    `json:"task_id"`
	State  string    `json:"state"`
	Result *Result   `json:"result,omitempty"`
	Upload *Manifest `json:"upload,omitempty"`
}

const MaxHeartbeatSeconds = 86400

type SyncRequest struct {
	ReplayHigh   uint64   `json:"replay_high,omitempty"`
	ReplayInit   bool     `json:"replay_init,omitempty"`
	Receipts     []string `json:"receipts,omitempty"`
	WaitSeconds  int      `json:"wait_seconds,omitempty"`
	LeaseSeconds int      `json:"lease_seconds,omitempty"`
	Version      int      `json:"version"`
	InstanceID   string   `json:"instance_id"`
	Node         Node     `json:"node"`
	Report       *Report  `json:"report,omitempty"`
	AcceptTask   bool     `json:"accept_task,omitempty"`
}

type SyncResponse struct {
	ReplayFloor  *uint64  `json:"replay_floor,omitempty"`
	Receipts     []string `json:"receipts,omitempty"`
	Version      int      `json:"version"`
	Task         *Task    `json:"task,omitempty"`
	Acknowledged string   `json:"acknowledged,omitempty"`
	CancelTaskID string   `json:"cancel_task_id,omitempty"`
}

type CreateTask struct {
	RequestID   string     `json:"request_id,omitempty"`
	CallerID    string     `json:"caller_id,omitempty"`
	StartBefore *time.Time `json:"start_before,omitempty"`
	NodeID      string     `json:"node_id"`
	Command     Command    `json:"command"`
}

type NodeCredential struct {
	ID    string `json:"id"`
	Token string `json:"token"`
}

type ControlConfig struct {
	Tunnel                  *TunnelServerConfig `json:"tunnel,omitempty"`
	AdminToken              string              `json:"admin_token"`
	Nodes                   []NodeCredential    `json:"nodes"`
	MaxActiveTasks          int                 `json:"max_active_tasks,omitempty"`
	MaxActiveBytes          int64               `json:"max_active_bytes,omitempty"`
	MaxHistoryTasks         int                 `json:"max_history_tasks,omitempty"`
	MaxHistoryBytes         int64               `json:"max_history_bytes,omitempty"`
	MaxConcurrentUploads    int                 `json:"max_concurrent_uploads,omitempty"`
	HistoryRetentionSeconds int64               `json:"history_retention_seconds,omitempty"`
}

// TunnelServerConfig configures the relay embedded in the hosted controller.
// Its key and reloadable ACL file are independent of management credentials.
type TunnelServerConfig struct {
	Listen           string `json:"listen"`
	KeyFile          string `json:"key_file"`
	AuthFile         string `json:"auth_file"`
	TLSCert          string `json:"tls_cert,omitempty"`
	TLSKey           string `json:"tls_key,omitempty"`
	Reverse          bool   `json:"reverse,omitempty"`
	Socks5           bool   `json:"socks5,omitempty"`
	KeepAliveSeconds *int   `json:"keep_alive_seconds,omitempty"`
}

type TaskPage struct {
	Tasks      []Task `json:"tasks"`
	NextCursor string `json:"next_cursor,omitempty"`
}

type NodeConfig struct {
	Tunnel                *TunnelConfig `json:"tunnel,omitempty"`
	Server                string        `json:"server"`
	NodeID                string        `json:"node_id"`
	Token                 string        `json:"token"`
	StateDir              string        `json:"state_dir"`
	WorkDir               string        `json:"work_dir"`
	StatePersistence      string        `json:"state_persistence,omitempty"`
	CAFile                string        `json:"ca_file,omitempty"`
	TLSProfile            string        `json:"tls_profile,omitempty"`
	TLSIdentityFile       string        `json:"tls_identity_file,omitempty"`
	TrafficShaping        string        `json:"traffic_shaping,omitempty"`
	BusyBox               string        `json:"busybox,omitempty"`
	HeartbeatSeconds      int           `json:"heartbeat_seconds,omitempty"`
	LeaseSeconds          int           `json:"lease_seconds,omitempty"`
	PollSeconds           int           `json:"poll_seconds,omitempty"`
	AllowInsecureLoopback bool          `json:"allow_insecure_loopback,omitempty"`
	MaxPendingResults     int           `json:"max_pending_results,omitempty"`
	MaxPendingResultBytes int           `json:"max_pending_result_bytes,omitempty"`
	UploadBytesPerSecond  int           `json:"upload_bytes_per_second,omitempty"`
}

// TunnelConfig configures the Chisel client embedded in the unified Node.
// The tunnel credential is independent of the management token.
type TunnelConfig struct {
	Server           string   `json:"server"`
	Auth             string   `json:"auth"`
	Fingerprint      string   `json:"fingerprint"`
	Remotes          []string `json:"remotes"`
	CAFile           string   `json:"ca_file,omitempty"`
	KeepAliveSeconds *int     `json:"keep_alive_seconds,omitempty"`
}
