package linklab

import (
	"bytes"
	"fmt"
	"math"
	"reflect"
	"testing"
	"time"
)

func model(t testing.TB, c Config) *Model {
	t.Helper()
	m, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	return m
}

func send(t testing.TB, m *Model, at time.Duration, peer string, d Direction, b []byte) bool {
	t.Helper()
	accepted, err := m.Submit(at, peer, d, b)
	if err != nil {
		t.Fatal(err)
	}
	return accepted
}

func TestScheduledOwnershipAndQueueBound(t *testing.T) {
	m := model(t, Config{QueueBytes: 6, Paths: [2]Path{{Delay: time.Second}, {}}})
	input := []byte("abc")
	if !send(t, m, 0, "a", Upload, input) || !send(t, m, 0, "a", Upload, input) || send(t, m, 0, "a", Upload, input) {
		t.Fatal("queue admission")
	}
	input[0] = 'z'
	if len(m.Drain(time.Second-1)) != 0 {
		t.Fatal("early delivery")
	}
	got := m.Drain(time.Second)
	if len(got) != 2 || string(got[0].Data) != "abc" || string(got[1].Data) != "abc" {
		t.Fatal(got)
	}
	if c := m.Snapshot(); c.QueuedBytes != 0 || c.PeakBytes != 6 || c.Overflow != 1 || c.Delivered != 2 {
		t.Fatal(c)
	}
	for _, slot := range m.queue[:cap(m.queue)] {
		if slot.Data != nil || slot.Peer != "" {
			t.Fatal("retained popped delivery")
		}
	}
	if !m.ReleasePeer("a") || len(m.peers) != 0 {
		t.Fatal("peer retained")
	}
}

func TestEmptyPacketsAndPeerChurnStayBounded(t *testing.T) {
	m := model(t, Config{QueueBytes: 2, Peers: 1})
	if !send(t, m, 0, "a", Upload, nil) || send(t, m, 0, "b", Upload, nil) || !send(t, m, 0, "a", Upload, nil) || send(t, m, 0, "a", Upload, nil) {
		t.Fatal("empty packet/peer cap")
	}
	if m.ReleasePeer("a") {
		t.Fatal("released queued peer")
	}
	m.Drain(0)
	m.ReleasePeer("a")
	if !send(t, m, 0, "b", Upload, nil) {
		t.Fatal("expired peer blocked replacement")
	}
	m.Close()
	if m.Snapshot().QueuedBytes != 0 || len(m.queue) != 0 || len(m.peers) != 0 {
		t.Fatal("close retained state")
	}
	if _, err := m.Submit(0, "b", Upload, nil); err == nil {
		t.Fatal("accepted after close")
	}
}

func TestPacketMetadataHasItsOwnBoundAndDrainCannotRewindClock(t *testing.T) {
	m := model(t, Config{QueueBytes: 1 << 20, QueuePackets: 3})
	for i := 0; i < 10; i++ {
		send(t, m, 0, "peer", Upload, nil)
	}
	if len(m.queue) != 3 || m.Snapshot().Overflow != 7 {
		t.Fatal("empty packet metadata escaped packet cap")
	}
	m.Drain(time.Second)
	if _, err := m.Submit(0, "peer", Upload, nil); err == nil {
		t.Fatal("submission rewound drained clock")
	}
}

func TestConfigurationAndClockValidation(t *testing.T) {
	for _, p := range []Path{{Loss: -.1}, {Loss: 1.1}, {Loss: math.NaN()}, {Burst: .5}, {Burst: math.Inf(1)}, {Delay: -1}, {Jitter: -1}, {Wander: -1}, {BytesPerSecond: -1}, {PeerBytesPerSecond: -1}, {BurstBytes: -1}} {
		if _, err := New(Config{Paths: [2]Path{p, {}}}); err == nil {
			t.Fatalf("accepted %+v", p)
		}
	}
	for _, c := range []Config{{Peers: -1}, {Peers: 4097}, {QueueBytes: -1}, {QueueBytes: 65 << 20}} {
		if _, err := New(c); err == nil {
			t.Fatal(c)
		}
	}
	m := model(t, Config{})
	send(t, m, time.Second, "a", Upload, nil)
	if _, err := m.Submit(0, "a", Upload, nil); err == nil {
		t.Fatal("reversed clock")
	}
	if err := m.SetLoss(Download, math.NaN(), 0); err == nil {
		t.Fatal("invalid live config")
	}
	if err := m.SetLoss(Direction(2), .5, 0); err == nil {
		t.Fatal("invalid direction")
	}
}

func trace(t *testing.T, c Config) ([]bool, []Delivery) {
	m := model(t, c)
	decisions := make([]bool, 4000)
	for i := range decisions {
		decisions[i] = send(t, m, time.Duration(i)*time.Millisecond, "a", Upload, []byte{byte(i)})
	}
	return decisions, m.Drain(time.Hour)
}

func TestSeededLossAndJitterAreRepeatable(t *testing.T) {
	c := Config{Seed: 871, Paths: [2]Path{{Loss: .27, Jitter: 12 * time.Millisecond, Delay: 15 * time.Millisecond}, {}}}
	a, x := trace(t, c)
	b, y := trace(t, c)
	if !reflect.DeepEqual(a, b) || !reflect.DeepEqual(x, y) {
		t.Fatal("seed did not reproduce decisions/times")
	}
	c.Seed++
	other, _ := trace(t, c)
	if reflect.DeepEqual(a, other) {
		t.Fatal("seed ignored")
	}
}

func TestLossFractionAndClustering(t *testing.T) {
	for _, burst := range []float64{0, 8} {
		m := model(t, Config{Seed: 173, Paths: [2]Path{{Loss: .2, Burst: burst}, {}}})
		lost, runs := 0, 0
		previous := false
		for i := 0; i < 50000; i++ {
			drop := !send(t, m, time.Duration(i), "a", Upload, nil)
			m.Drain(time.Duration(i))
			if drop {
				lost++
				if !previous {
					runs++
				}
			}
			previous = drop
		}
		fraction := float64(lost) / 50000
		length := float64(lost) / float64(runs)
		if math.Abs(fraction-.2) > .035 {
			t.Fatalf("burst=%g fraction=%g", burst, fraction)
		}
		if burst == 0 && length > 1.5 || burst > 0 && (length < 6 || length > 10) {
			t.Fatalf("burst=%g run=%g", burst, length)
		}
	}
}

func TestDirectionAndLiveLossChanges(t *testing.T) {
	m := model(t, Config{Paths: [2]Path{{Loss: 1}, {}}})
	for i := 0; i < 10; i++ {
		if send(t, m, 0, "a", Upload, nil) || !send(t, m, 0, "a", Download, nil) {
			t.Fatal("asymmetric loss")
		}
	}
	if err := m.SetLoss(Upload, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := m.SetLoss(Download, 1, 0); err != nil {
		t.Fatal(err)
	}
	if !send(t, m, 0, "a", Upload, nil) || send(t, m, 0, "a", Download, nil) {
		t.Fatal("live loss unchanged")
	}
}

func TestDelayOrderWanderAndJitter(t *testing.T) {
	for _, jitter := range []time.Duration{0, 20 * time.Millisecond} {
		m := model(t, Config{Seed: 79, Paths: [2]Path{{Delay: 30 * time.Millisecond, Wander: 10 * time.Millisecond, Jitter: jitter}, {}}})
		for i := 0; i < 400; i++ {
			send(t, m, time.Duration(i)*time.Millisecond, "a", Upload, nil)
		}
		got := m.Drain(time.Hour)
		reordered, changed := false, false
		for i, v := range got {
			if v.At < time.Duration(v.serial-1)*time.Millisecond {
				t.Fatal("negative latency")
			}
			if i > 0 && v.serial < got[i-1].serial {
				reordered = true
			}
			if v.At-time.Duration(v.serial-1)*time.Millisecond != 30*time.Millisecond {
				changed = true
			}
		}
		if !changed || reordered != (jitter > 0) {
			t.Fatalf("jitter=%s changed=%v reordered=%v", jitter, changed, reordered)
		}
	}
}

func TestAggregateAndPerPeerSerialization(t *testing.T) {
	for _, tc := range []struct {
		name string
		path Path
		want []time.Duration
	}{
		{"shared", Path{BytesPerSecond: 1000}, []time.Duration{time.Second, 2 * time.Second, 3 * time.Second, 4 * time.Second}},
		{"per-peer", Path{PeerBytesPerSecond: 1000}, []time.Duration{time.Second, time.Second, 2 * time.Second, 2 * time.Second}},
		{"both", Path{BytesPerSecond: 2000, PeerBytesPerSecond: 1000}, []time.Duration{time.Second, time.Second, 2 * time.Second, 2 * time.Second}},
		{"burst", Path{BytesPerSecond: 1000, BurstBytes: 2000}, []time.Duration{0, 0, time.Second, 2 * time.Second}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := model(t, Config{Paths: [2]Path{tc.path, {}}})
			for _, peer := range []string{"a", "b", "a", "b"} {
				send(t, m, 0, peer, Upload, make([]byte, 1000))
			}
			got := m.Drain(time.Hour)
			for i, v := range got {
				if v.At != tc.want[i] {
					t.Fatalf("packet %d at %v want %v", i, v.At, tc.want[i])
				}
			}
		})
	}
}

func TestLossAfterBottleneckStillConsumesCapacity(t *testing.T) {
	for _, after := range []bool{false, true} {
		m := model(t, Config{Paths: [2]Path{{Loss: 1, LossAfterQueue: after, BytesPerSecond: 1000}, {}}})
		send(t, m, 0, "a", Upload, make([]byte, 1000))
		m.SetLoss(Upload, 0, 0)
		send(t, m, 0, "a", Upload, make([]byte, 1000))
		got := m.Drain(time.Hour)
		want := time.Second
		if after {
			want *= 2
		}
		if len(got) != 1 || got[0].At != want {
			t.Fatal(after, got)
		}
	}
}

func TestPerPeerPolicerCannotBorrowAnotherPeersCredit(t *testing.T) {
	m := model(t, Config{Paths: [2]Path{{PeerBytesPerSecond: 1000, PolicePerPeer: true, BurstBytes: 1000}, {}}})
	packet := make([]byte, 1000)
	if !send(t, m, 0, "a", Upload, packet) || send(t, m, 0, "a", Upload, packet) || !send(t, m, 0, "b", Upload, packet) {
		t.Fatal("per-peer burst credit shared or unlimited")
	}
	if len(m.Drain(0)) != 2 {
		t.Fatal("policer unexpectedly delayed admitted packets")
	}
	if !send(t, m, 500*time.Millisecond, "a", Upload, packet[:500]) || send(t, m, 500*time.Millisecond, "a", Upload, packet[:500]) {
		t.Fatal("policer refill rate incorrect")
	}
	m.Drain(time.Second)
	if !send(t, m, time.Second, "a", Upload, packet[:500]) || m.Snapshot().Policed != 2 {
		t.Fatal("refill/loss accounting", m.Snapshot())
	}
}

func TestQueueReusesBoundedStorageAcrossCampaign(t *testing.T) {
	m := model(t, Config{QueueBytes: 4096})
	data := bytes.Repeat([]byte{3}, 1024)
	for i := 0; i < 2000; i++ {
		for j := 0; j < 8; j++ {
			send(t, m, time.Duration(i), "a", Upload, data)
		}
		m.Drain(time.Duration(i))
	}
	if m.Snapshot().PeakBytes != 4096 || cap(m.queue) > 8 || len(m.peers) != 1 {
		t.Fatal(m.Snapshot(), cap(m.queue))
	}
}

func BenchmarkPacketCampaign(b *testing.B) {
	for _, size := range []int{64, 1200, 8192} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			m := model(b, Config{Paths: [2]Path{{Loss: .1, Burst: 4, Delay: time.Millisecond}, {}}})
			packet := make([]byte, size)
			b.ReportAllocs()
			b.SetBytes(int64(size))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				now := time.Duration(i) * time.Millisecond
				m.Drain(now)
				send(b, m, now, "peer", Upload, packet)
			}
			b.StopTimer()
			b.ReportMetric(float64(m.Snapshot().PeakBytes), "peak-queued-B")
		})
	}
}

func FuzzPacketSchedule(f *testing.F) {
	f.Add([]byte{0, 1, 2, 254, 255})
	f.Fuzz(func(t *testing.T, input []byte) {
		if len(input) > 4096 {
			t.Skip()
		}
		m := model(t, Config{QueueBytes: 128, Peers: 4, Seed: 42, Paths: [2]Path{{Loss: .2, Jitter: 3}, {Loss: .8, Burst: 5}}})
		for i, v := range input {
			send(t, m, time.Duration(i), fmt.Sprint(v%8), Direction(v%2), input[:int(v)%max(1, len(input))])
			m.Drain(time.Duration(i))
			if c := m.Snapshot(); c.QueuedBytes < 0 || c.QueuedBytes > 128 || c.PeakBytes > 128 {
				t.Fatal(c)
			}
		}
		m.Drain(time.Hour)
		if m.Snapshot().QueuedBytes != 0 {
			t.Fatal("queue not drained")
		}
	})
}
