package telemetry

import "testing"

func BenchmarkSnapshot(b *testing.B) {
	m := New()
	m.Snapshot() // Include no one-time runtime metric initialization in the loop.
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		v := m.Snapshot()
		if v.GoAllocBytes == 0 || v.Goroutines == 0 {
			b.Fatal("missing runtime metrics")
		}
	}
}
