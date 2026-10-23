package telemetry

import (
	"sync"
	"testing"
	"time"
)

func TestConcurrentOutageCountersAndRecovery(t *testing.T) {
	m := New()
	var workers sync.WaitGroup
	for worker := 0; worker < 16; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for i := 0; i < 100; i++ {
				m.Request(false, time.Now(), 503, "http")
				m.Request(true, time.Now(), 0, "timeout")
				if i%10 == 0 {
					v := m.Snapshot()
					if v.GoAllocBytes == 0 || v.Goroutines == 0 {
						t.Error("missing concurrent runtime snapshot")
					}
				}
			}
		}()
	}
	workers.Wait()
	v := m.Snapshot()
	if v.Control.Failures != 1600 || v.Upload.Failures != 1600 || v.Control.ConsecutiveFailures != 1600 || v.Upload.Requests != 1600 {
		t.Fatal(v)
	}
	m.Request(false, time.Now(), 200, "")
	m.Request(true, time.Now(), 0, "canceled")
	v = m.Snapshot()
	if v.Control.ConsecutiveFailures != 0 || v.Control.Failures != 1600 || v.Control.LastSuccessAt.IsZero() || v.Upload.Failures != 1600 || v.Upload.Canceled != 1 {
		t.Fatal(v)
	}
	m.Request(false, time.Now(), 0, "dns")
	if v = m.Snapshot(); v.Control.ConsecutiveFailures != 1 || v.Control.Failures != 1601 || v.Control.LastError != "dns" {
		t.Fatal(v)
	}
}

func BenchmarkOutageCounters(b *testing.B) {
	m := New()
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			m.Request(false, time.Now(), 503, "http")
		}
	})
}
