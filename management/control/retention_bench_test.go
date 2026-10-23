package control

import (
	"os"
	"path/filepath"
	"testing"

	p "tack.local/management/protocol"
)

func BenchmarkHistoryReceiptOwner(b *testing.B) {
	for _, size := range []int{0, p.MaxOutput} {
		name := "empty"
		if size > 0 {
			name = "2MiB"
		}
		b.Run(name, func(b *testing.B) {
			s := &Server{path: filepath.Join(b.TempDir(), "state.json")}
			os.MkdirAll(s.path+".history", 0700)
			task := p.Task{ID: "benchmark", NodeID: "node-a", State: "succeeded", Result: &p.Result{Stdout: make([]byte, size), Stderr: make([]byte, size)}}
			if err := p.WriteJSON(s.historyPath(task.ID), task); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				owner, err := s.receiptOwner(task.ID)
				if err != nil || owner != "node-a" {
					b.Fatal(owner, err)
				}
			}
		})
	}
}
