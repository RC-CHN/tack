// Package telemetry collects bounded, process-local counters without task contents.
package telemetry

import (
	"encoding/json"
	"runtime"
	"runtime/metrics"
	"sync"
	"time"

	p "tack.local/management/protocol"
)

type Meter struct {
	mu   sync.Mutex
	data p.RuntimeMetrics
	heap [1]metrics.Sample
}

func New() *Meter { return &Meter{data: p.RuntimeMetrics{StartedAt: time.Now().UTC()}} }
func (m *Meter) Request(upload bool, start time.Time, status int, failure string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := &m.data.Control
	if upload {
		r = &m.data.Upload
	}
	r.Requests++
	r.LastDurationMS = time.Since(start).Milliseconds()
	r.TotalDurationMS += r.LastDurationMS
	if r.LastDurationMS > r.MaxDurationMS {
		r.MaxDurationMS = r.LastDurationMS
	}
	r.LastStatus, r.LastError = status, failure
	if failure == "canceled" {
		r.Canceled++
	} else if failure != "" {
		r.Failures++
		r.ConsecutiveFailures++
	} else {
		r.ConsecutiveFailures = 0
		r.LastSuccessAt = time.Now().UTC()
	}
}
func (m *Meter) WriteJSON(path string, value any) error {
	start := time.Now()
	// Durable state is machine-readable. Avoid a second whole-value buffer and
	// indentation pass, particularly for archived base64 task output.
	b, err := json.Marshal(value)
	if err == nil {
		b = append(b, '\n')
		err = p.WriteFile(path, b)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data.StateWrites++
	if err != nil {
		m.data.StateWriteFailures++
	} else {
		m.data.StateBytesWritten += uint64(len(b))
	}
	ms := time.Since(start).Milliseconds()
	m.data.StateWriteMS += ms
	if ms > m.data.StateWriteMaxMS {
		m.data.StateWriteMaxMS = ms
	}
	return err
}
func (m *Meter) Snapshot() p.RuntimeMetrics {
	m.mu.Lock()
	// Read only the heap gauge we publish, without ReadMemStats' global pause.
	// Reuse the sample under the same lock: metrics.Read mutates its values.
	m.heap[0].Name = "/memory/classes/heap/objects:bytes"
	metrics.Read(m.heap[:])
	v := m.data
	v.GoAllocBytes = m.heap[0].Value.Uint64()
	m.mu.Unlock()
	v.UptimeSeconds = int64(time.Since(v.StartedAt).Seconds())
	v.Goroutines = runtime.NumGoroutine()
	v.CPUTimeMS, v.CPUAvailable = cpuTime()
	return v
}
