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

// NodeSyncPath retains the version 1 wire endpoint across the Tack rename.
const NodeSyncPath = "/v1/agent/sync"

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
	ID              string    `json:"id"`
	NodeID          string    `json:"node_id"`
	Command         Command   `json:"command"`
	CreatedAt       time.Time `json:"created_at"`
	State           string    `json:"state"`
	CancelRequested bool      `json:"cancel_requested"`
	Result          *Result   `json:"result,omitempty"`
}

type Result struct {
	StartedAt       time.Time `json:"started_at"`
	FinishedAt      time.Time `json:"finished_at"`
	Executable      string    `json:"executable,omitempty"`
	ExitCode        *int      `json:"exit_code,omitempty"`
	Stdout          []byte    `json:"stdout,omitempty"`
	Stderr          []byte    `json:"stderr,omitempty"`
	StdoutTruncated bool      `json:"stdout_truncated"`
	StderrTruncated bool      `json:"stderr_truncated"`
	Error           string    `json:"error,omitempty"`
}

func Terminal(state string) bool {
	switch state {
	case "succeeded", "failed", "timed_out", "canceled", "unknown":
		return true
	}
	return false
}

type Node struct {
	ID               string    `json:"id"`
	Hostname         string    `json:"hostname"`
	OS               string    `json:"os"`
	Arch             string    `json:"arch"`
	Modes            []string  `json:"modes"`
	StatePersistence string    `json:"state_persistence"`
	LastSeen         time.Time `json:"last_seen"`
}

type Report struct {
	TaskID string  `json:"task_id"`
	State  string  `json:"state"`
	Result *Result `json:"result,omitempty"`
}

type SyncRequest struct {
	Version    int     `json:"version"`
	InstanceID string  `json:"instance_id"`
	Node       Node    `json:"node"`
	Report     *Report `json:"report,omitempty"`
}

type SyncResponse struct {
	Version      int    `json:"version"`
	Task         *Task  `json:"task,omitempty"`
	Acknowledged string `json:"acknowledged,omitempty"`
	CancelTaskID string `json:"cancel_task_id,omitempty"`
}

type CreateTask struct {
	NodeID  string  `json:"node_id"`
	Command Command `json:"command"`
}

type NodeCredential struct {
	ID    string `json:"id"`
	Token string `json:"token"`
}

type ControlConfig struct {
	AdminToken string           `json:"admin_token"`
	Nodes      []NodeCredential `json:"nodes"`
}

type NodeConfig struct {
	Server                string `json:"server"`
	NodeID                string `json:"node_id"`
	Token                 string `json:"token"`
	StateDir              string `json:"state_dir"`
	WorkDir               string `json:"work_dir"`
	StatePersistence      string `json:"state_persistence,omitempty"`
	CAFile                string `json:"ca_file,omitempty"`
	BusyBox               string `json:"busybox,omitempty"`
	PollSeconds           int    `json:"poll_seconds,omitempty"`
	AllowInsecureLoopback bool   `json:"allow_insecure_loopback,omitempty"`
}
