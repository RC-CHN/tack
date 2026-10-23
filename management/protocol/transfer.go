package protocol

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"
)

// Manifest describes the concatenation of stdout and stderr, without JSON/base64.
type Manifest struct {
	StdoutBytes int    `json:"stdout_bytes"`
	StderrBytes int    `json:"stderr_bytes"`
	SHA256      string `json:"sha256"`
}

func (m Manifest) Total() int { return m.StdoutBytes + m.StderrBytes }
func (m Manifest) Validate(limit int) error {
	if m.StdoutBytes < 0 || m.StderrBytes < 0 || m.StdoutBytes > limit || m.StderrBytes > limit {
		return errors.New("invalid output lengths")
	}
	b, err := hex.DecodeString(m.SHA256)
	if err != nil || len(b) != sha256.Size || hex.EncodeToString(b) != m.SHA256 {
		return errors.New("invalid output digest")
	}
	return nil
}
func OutputManifest(r *Result) Manifest {
	h := sha256.New()
	h.Write(r.Stdout)
	h.Write(r.Stderr)
	return Manifest{StdoutBytes: len(r.Stdout), StderrBytes: len(r.Stderr), SHA256: hex.EncodeToString(h.Sum(nil))}
}

type ResultChunk struct {
	Version    int    `json:"version"`
	NodeID     string `json:"node_id"`
	InstanceID string `json:"instance_id"`
	TaskID     string `json:"task_id"`
	SHA256     string `json:"sha256"`
	Probe      bool   `json:"probe,omitempty"`
	Offset     int    `json:"offset"`
	Data       []byte `json:"data,omitempty"`
}
type ChunkAck struct {
	Version  int    `json:"version"`
	TaskID   string `json:"task_id"`
	Offset   int    `json:"offset"`
	Complete bool   `json:"complete"`
}

// All observations use the controller's clock. They include polling/network delay.
type TaskTiming struct {
	DispatchedAt        time.Time `json:"dispatched_at"`
	RunningReportedAt   time.Time `json:"running_reported_at"`
	CancelRequestedAt   time.Time `json:"cancel_requested_at"`
	TerminalReportedAt  time.Time `json:"terminal_reported_at"`
	ResultReceivedAt    time.Time `json:"result_received_at"`
	QueueMS             int64     `json:"queue_ms"`
	DispatchToRunningMS int64     `json:"dispatch_to_running_ms"`
	CancelToTerminalMS  int64     `json:"cancel_to_terminal_ms"`
	ResultTransferMS    int64     `json:"result_transfer_ms"`
}
