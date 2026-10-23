package runner

import (
	"bytes"
	"math/rand"
	"testing"
)

func TestHeadTailRingAcrossWrites(t *testing.T) {
	r := rand.New(rand.NewSource(19))
	for limit := 0; limit < 129; limit++ {
		b := &bounded{limit: limit}
		var all []byte
		for j := 0; j < 80; j++ {
			v := make([]byte, r.Intn(17))
			r.Read(v)
			all = append(all, v...)
			if n, err := b.Write(v); err != nil || n != len(v) {
				t.Fatal(n, err)
			}
			got, truncated := b.snapshot()
			want := all
			if len(all) > limit {
				head := (limit + 1) / 2
				tail := limit - head
				want = append(append([]byte{}, all[:head]...), all[len(all)-tail:]...)
			}
			if !bytes.Equal(got, want) || truncated != (len(all) > limit) || cap(b.data) > limit {
				t.Fatalf("limit=%d step=%d got=%x want=%x truncated=%v", limit, j, got, want, truncated)
			}
		}
		want, truncated := b.snapshot()
		got, cut := b.finish()
		if !bytes.Equal(got, want) || cut != truncated || b.data != nil {
			t.Fatal("finish changed head/tail order or retained ownership")
		}
		b.Write([]byte("new output"))
		if !bytes.Equal(got, want) {
			t.Fatal("subsequent writes changed the detached result")
		}
	}
}

func BenchmarkBoundedOutput(b *testing.B) {
	for _, size := range []int{12, 1024 * 1024} {
		name := "short"
		if size > 12 {
			name = "large"
		}
		b.Run(name, func(b *testing.B) {
			input := bytes.Repeat([]byte("x"), size)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				buf := &bounded{limit: 65536}
				buf.Write(input)
				buf.finish()
			}
		})
	}
}

func TestShortOutputAllocatesOnlyOnDemand(t *testing.T) {
	b := &bounded{limit: 2 * 1024 * 1024}
	b.Write(nil)
	if len(b.data) != 0 {
		t.Fatal("empty output allocated storage")
	}
	b.Write([]byte("short output"))
	if cap(b.data) > 64 {
		t.Fatalf("short output allocated %d bytes", cap(b.data))
	}
}
