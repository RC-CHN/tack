package runner

import (
	"bytes"
	"fmt"
	"sync"
	"testing"
)

// Exercise the production output owner under actual concurrent producers.
// The command runner uses independent stdout/stderr owners, each with a cap.
func TestConcurrentOutputBudgetAndOwnershipTransfer(t *testing.T) {
	for _, limit := range []int{1, 511, 64 << 10, 2 << 20} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			output := &bounded{limit: limit}
			input := bytes.Repeat([]byte{'r'}, 8192)
			var writers sync.WaitGroup
			for worker := 0; worker < 12; worker++ {
				writers.Add(1)
				go func() {
					defer writers.Done()
					for i := 0; i < 48; i++ {
						if n, err := output.Write(input); n != len(input) || err != nil {
							t.Errorf("write: %d %v", n, err)
						}
						output.mu.Lock()
						capacity := cap(output.data)
						output.mu.Unlock()
						if capacity > limit {
							t.Errorf("capacity %d > %d", capacity, limit)
						}
					}
				}()
			}
			writers.Wait()
			backing := output.data
			result, cut := output.finish()
			if !cut || len(result) != limit || cap(result) != limit || &result[0] != &backing[0] {
				t.Fatal("finish copied/unbounded output")
			}
			if output.data != nil {
				t.Fatal("owner retained finished output")
			}
			output.Write([]byte("next"))
			output.finish()
			if !bytes.Equal(result, bytes.Repeat([]byte{'r'}, limit)) {
				t.Fatal("new result mutated previously transferred bytes")
			}
		})
	}
}

func BenchmarkOutputChurn(b *testing.B) {
	for _, limit := range []int{16 << 10, 2 << 20} {
		b.Run(fmt.Sprint(limit), func(b *testing.B) {
			input := bytes.Repeat([]byte{'x'}, 4096)
			b.ReportAllocs()
			b.SetBytes(int64(limit * 2))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				out := &bounded{limit: limit}
				for n := 0; n < limit*2; n += len(input) {
					out.Write(input)
				}
				data, cut := out.finish()
				if !cut || len(data) != limit {
					b.Fatal("unbounded output")
				}
			}
			b.ReportMetric(float64(limit), "retained-B/result")
		})
	}
}
