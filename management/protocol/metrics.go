package protocol

import "time"

type RequestMetrics struct {
	Canceled            uint64    `json:"canceled"`
	Requests            uint64    `json:"requests"`
	Failures            uint64    `json:"failures"`
	ConsecutiveFailures uint64    `json:"consecutive_failures"`
	LastDurationMS      int64     `json:"last_duration_ms"`
	TotalDurationMS     int64     `json:"total_duration_ms"`
	MaxDurationMS       int64     `json:"max_duration_ms"`
	LastStatus          int       `json:"last_status"`
	LastError           string    `json:"last_error,omitempty"`
	LastSuccessAt       time.Time `json:"last_success_at"`
}
type RuntimeMetrics struct {
	StartedAt          time.Time      `json:"started_at"`
	UptimeSeconds      int64          `json:"uptime_seconds"`
	GoAllocBytes       uint64         `json:"go_alloc_bytes"`
	Goroutines         int            `json:"goroutines"`
	CPUTimeMS          int64          `json:"cpu_time_ms"`
	CPUAvailable       bool           `json:"cpu_available"`
	Control            RequestMetrics `json:"control"`
	Upload             RequestMetrics `json:"upload"`
	StateWrites        uint64         `json:"state_writes"`
	StateWriteFailures uint64         `json:"state_write_failures"`
	StateBytesWritten  uint64         `json:"state_bytes_written"`
	StateWriteMS       int64          `json:"state_write_ms"`
	StateWriteMaxMS    int64          `json:"state_write_max_ms"`
}
type NodeMetrics struct {
	Runtime            RuntimeMetrics `json:"runtime"`
	JournalRecords     int            `json:"journal_records"`
	PendingResults     int            `json:"pending_results"`
	PendingResultBytes int            `json:"pending_result_bytes"`
	AcceptingTasks     bool           `json:"accepting_tasks"`
}
type MetricsSnapshot struct {
	Controller RuntimeMetrics `json:"controller"`
	Nodes      []Node         `json:"nodes"`
}
