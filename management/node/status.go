package node

import (
	"context"
	"encoding/json"
	"os"
	"runtime"
	p "tack.local/management/protocol"
	"time"
)

func (a *Node) notifyStatus() {
	select {
	case a.statusWake <- struct{}{}:
	default:
	}
}
func (a *Node) setTransfer(task, code, phase, message string) {
	a.statusMu.Lock()
	previous := a.transfer
	// Notify only on a state change, not on every retry or output chunk.
	if previous != nil && previous.TaskID == task && previous.ErrorCode == code && previous.Phase == phase && previous.Error == message {
		a.statusMu.Unlock()
		return
	}
	a.transfer = &p.TransferStatus{TaskID: task, ErrorCode: code, Phase: phase, Error: message, UpdatedAt: time.Now().UTC()}
	a.statusMu.Unlock()
	a.notifyStatus()
}
func (a *Node) statusSnapshot(m p.NodeMetrics) *p.NodeStatus {
	a.statusMu.Lock()
	s := &p.NodeStatus{AcceptingTasks: m.AcceptingTasks, CurrentTask: a.currentTask, Transfer: a.transfer, UpdatedAt: time.Now().UTC(), Tunnel: p.TunnelStatus{State: "disabled"}}
	a.statusMu.Unlock()
	if a.TunnelStatus != nil {
		s.Tunnel = a.TunnelStatus()
	}
	switch {
	case s.CurrentTask != "":
		s.BacklogReason = "task_running"
	case m.JournalRecords >= 10000:
		s.BacklogReason = "receipt_backlog"
	case !m.AcceptingTasks:
		s.BacklogReason = "result_backlog"
	}
	return s
}

func boundResultMetadata(result *p.Result) {
	result.Error, result.MetadataTruncated = boundedMetadata(result.Error, 8192, result.MetadataTruncated)
	result.Executable, result.MetadataTruncated = boundedMetadata(result.Executable, 4096, result.MetadataTruncated)
	if result.Cleanup != nil {
		c := *result.Cleanup
		c.Error, result.MetadataTruncated = boundedMetadata(c.Error, 4096, result.MetadataTruncated)
		c.Path, result.MetadataTruncated = boundedMetadata(c.Path, 4096, result.MetadataTruncated)
		result.Cleanup = &c
	}
	// JSON escaping can expand control characters by six times. Reserve enough
	// space for the spool header even for adversarial filenames/error text.
	size := len(result.Error) + len(result.Executable)
	if result.Cleanup != nil {
		size += len(result.Cleanup.Error) + len(result.Cleanup.Path)
	}
	if size <= 8000 {
		return
	}
	metadata := *result
	metadata.Stdout, metadata.Stderr = nil, nil
	for {
		b, _ := json.Marshal(metadata)
		if len(b) <= 48*1024 {
			break
		}
		shrink := func(value string) string {
			if len(value) <= 256 {
				return value
			}
			v, _ := boundedMetadata(value, len(value)/2, true)
			return v
		}
		metadata.Error = shrink(metadata.Error)
		metadata.Executable = shrink(metadata.Executable)
		if metadata.Cleanup != nil {
			metadata.Cleanup.Error = shrink(metadata.Cleanup.Error)
			metadata.Cleanup.Path = shrink(metadata.Cleanup.Path)
		}
		metadata.MetadataTruncated = true
	}
	result.Error, result.Executable, result.Cleanup, result.MetadataTruncated = metadata.Error, metadata.Executable, metadata.Cleanup, metadata.MetadataTruncated
}

// The executor keeps completed output in memory on a disk failure and must not
// accept more work. A bounded status-only call makes that reason observable
// without acknowledging unpersisted results or blocking the retry indefinitely.
func (a *Node) reportStorageFailure(parent context.Context, metrics p.NodeMetrics) {
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	defer cancel()
	host, _ := os.Hostname()
	status := a.statusSnapshot(metrics)
	status.AcceptingTasks = false
	status.CurrentTask = ""
	status.BacklogReason = "result_persistence_failed"
	in := p.SyncRequest{Version: p.OperationsVersion, InstanceID: a.instance, LeaseSeconds: a.config.LeaseSeconds, Node: p.Node{ID: a.config.NodeID, Hostname: host, OS: runtime.GOOS, Arch: runtime.GOARCH, Modes: a.runner.Modes(), StatePersistence: a.config.StatePersistence, Status: status}}
	var out p.SyncResponse
	_ = a.request(ctx, a.client, p.NodeOperationsPath, in, &out)
}
