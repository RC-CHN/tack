// Package linklab provides a bounded, explicit-clock datagram experiment.
// It is an independent implementation of test/fixtures/BEHAVIOR.md.
package linklab

import (
	"container/heap"
	"errors"
	"math"
	"math/rand"
	"time"
)

type Direction uint8

const (
	Upload Direction = iota
	Download
)

type Path struct {
	Delay, Jitter time.Duration
	// Wander smooths delay changes and preserves order when Jitter is zero.
	Wander time.Duration
	Loss   float64
	// Burst is the mean length of loss runs; zero means independent loss.
	Burst              float64
	BytesPerSecond     int64
	PeerBytesPerSecond int64
	// PolicePerPeer drops excess traffic instead of delaying it. BurstBytes
	// must cover the desired admitted burst when this option is enabled.
	PolicePerPeer bool
	BurstBytes    int
	// LossAfterQueue consumes serialization time before applying random loss.
	LossAfterQueue bool
}

type Config struct {
	Paths                           [2]Path
	Seed                            int64
	QueueBytes, QueuePackets, Peers int
}

type Counts struct {
	Submitted, Delivered, Lost, Overflow, Policed uint64
	QueuedBytes, PeakBytes                        int
}

type Delivery struct {
	At        time.Duration
	Peer      string
	Direction Direction
	Data      []byte
	serial    uint64
}

type events []Delivery

func (q events) Len() int { return len(q) }
func (q events) Less(i, j int) bool {
	if q[i].At == q[j].At {
		return q[i].serial < q[j].serial
	}
	return q[i].At < q[j].At
}
func (q events) Swap(i, j int) { q[i], q[j] = q[j], q[i] }
func (q *events) Push(v any)   { *q = append(*q, v.(Delivery)) }
func (q *events) Pop() any {
	a := *q
	i := len(a) - 1
	v := a[i]
	a[i] = Delivery{}
	*q = a[:i]
	return v
}

type peer struct {
	next   [2]time.Duration
	queued int
}
type channel struct {
	next, ordered, wander time.Duration
	dropping              bool
}

// Model is deliberately single-owner, so simulations need neither locks nor
// background goroutines. Call Drain before advancing Submit's clock.
type Model struct {
	config   Config
	rng      *rand.Rand
	clock    time.Duration
	peers    map[string]*peer
	channels [2]channel
	queue    events
	counts   Counts
	closed   bool
}

func valid(p Path) bool {
	return p.Delay >= 0 && p.Jitter >= 0 && p.Wander >= 0 &&
		p.Delay <= time.Hour && p.Jitter <= time.Hour && p.Wander <= time.Hour &&
		!math.IsNaN(p.Loss) && p.Loss >= 0 && p.Loss <= 1 &&
		!math.IsNaN(p.Burst) && !math.IsInf(p.Burst, 0) && (p.Burst == 0 || p.Burst >= 1) &&
		p.BytesPerSecond >= 0 && p.PeerBytesPerSecond >= 0 && p.BurstBytes >= 0 && p.BurstBytes <= 64<<20
}

func New(c Config) (*Model, error) {
	if c.QueueBytes == 0 {
		c.QueueBytes = 256 << 10
	}
	if c.Peers == 0 {
		c.Peers = 32
	}
	if c.QueuePackets == 0 {
		c.QueuePackets = 1024
	}
	if c.QueueBytes < 1 || c.QueueBytes > 64<<20 || c.QueuePackets < 1 || c.QueuePackets > 65536 || c.Peers < 1 || c.Peers > 4096 || !valid(c.Paths[0]) || !valid(c.Paths[1]) {
		return nil, errors.New("invalid bounded link configuration")
	}
	m := &Model{config: c, rng: rand.New(rand.NewSource(c.Seed)), peers: make(map[string]*peer)}
	for d, p := range c.Paths {
		m.channels[d].next = -credit(p.BurstBytes, p.BytesPerSecond)
	}
	return m, nil
}

func (m *Model) Snapshot() Counts { return m.counts }

func (m *Model) SetLoss(d Direction, fraction, burst float64) error {
	if d > Download {
		return errors.New("invalid direction")
	}
	p := m.config.Paths[d]
	p.Loss, p.Burst = fraction, burst
	if !valid(p) {
		return errors.New("invalid loss")
	}
	m.config.Paths[d] = p
	m.channels[d].dropping = false
	return nil
}

func (m *Model) lost(d Direction) bool {
	p, c := m.config.Paths[d], &m.channels[d]
	if p.Loss == 0 {
		return false
	}
	if p.Loss == 1 {
		return true
	}
	if p.Burst == 0 {
		return m.rng.Float64() < p.Loss
	}
	// Stationary probability = entry/(entry+exit). Reduce both transition
	// probabilities when necessary so high-loss configurations remain valid.
	exit := 1 / p.Burst
	entry := exit * p.Loss / (1 - p.Loss)
	if entry > 1 {
		exit /= entry
		entry = 1
	}
	if c.dropping {
		c.dropping = m.rng.Float64() >= exit
	} else {
		c.dropping = m.rng.Float64() < entry
	}
	return c.dropping
}

func credit(bytes int, rate int64) time.Duration {
	if rate == 0 {
		return 0
	}
	return time.Duration(float64(bytes) * float64(time.Second) / float64(rate))
}

func serialize(now time.Duration, next *time.Duration, n int, rate int64, allowance int) time.Duration {
	if rate == 0 {
		return now
	}
	window := credit(allowance, rate)
	if *next < now-window {
		*next = now - window
	}
	*next += time.Duration(math.Ceil(float64(n) * float64(time.Second) / float64(rate)))
	if *next < now {
		return now
	}
	return *next
}

// Submit copies only admitted payloads. false means an intentional loss or a
// resource rejection; errors describe invalid input. Empty datagrams are valid.
func (m *Model) Submit(at time.Duration, name string, d Direction, data []byte) (bool, error) {
	if m.closed || at < m.clock || at < 0 || at > time.Duration(1<<62) || d > Download || len(name) == 0 || len(name) > 256 || len(data) > 65507 {
		return false, errors.New("closed model or invalid packet/clock")
	}
	m.clock = at
	m.counts.Submitted++
	p, c := m.config.Paths[d], &m.channels[d]
	size := max(1, len(data)) // Empty packets also consume queue capacity.
	s := m.peers[name]
	if len(m.queue) >= m.config.QueuePackets || size > m.config.QueueBytes-m.counts.QueuedBytes || (s == nil && len(m.peers) == m.config.Peers) {
		m.counts.Overflow++
		return false, nil
	}
	if !p.LossAfterQueue && m.lost(d) {
		m.counts.Lost++
		return false, nil
	}
	if s == nil {
		s = &peer{}
		for direction, path := range m.config.Paths {
			s.next[direction] = -credit(path.BurstBytes, path.PeerBytesPerSecond)
		}
		m.peers[name] = s
	}
	peerNext := s.next[d]
	peerReady := serialize(at, &peerNext, size, p.PeerBytesPerSecond, p.BurstBytes)
	if p.PolicePerPeer && peerReady > at {
		m.counts.Policed++
		return false, nil
	}
	s.next[d] = peerNext
	ready := serialize(at, &c.next, size, p.BytesPerSecond, p.BurstBytes)
	ready = max(ready, peerReady)
	if p.LossAfterQueue && m.lost(d) {
		m.counts.Lost++
		return false, nil
	}
	delay := p.Delay
	if p.Wander > 0 {
		step := time.Duration(m.rng.Int63n(int64(p.Wander)*2+1)) - p.Wander
		c.wander = (3*c.wander + step) / 4
		delay += c.wander
	}
	if p.Jitter > 0 {
		delay += time.Duration(m.rng.Int63n(int64(p.Jitter)*2+1)) - p.Jitter
	}
	ready += max(0, delay)
	if p.Jitter == 0 {
		ready = max(ready, c.ordered)
		c.ordered = ready
	}
	s.queued++
	m.counts.QueuedBytes += size
	m.counts.PeakBytes = max(m.counts.PeakBytes, m.counts.QueuedBytes)
	heap.Push(&m.queue, Delivery{ready, name, d, append([]byte(nil), data...), m.counts.Submitted})
	return true, nil
}

// Drain transfers ownership of deliveries up to at to the caller.
func (m *Model) Drain(at time.Duration) []Delivery {
	m.clock = max(m.clock, at)
	var out []Delivery
	for len(m.queue) > 0 && m.queue[0].At <= at {
		v := heap.Pop(&m.queue).(Delivery)
		m.counts.QueuedBytes -= max(1, len(v.Data))
		m.counts.Delivered++
		m.peers[v.Peer].queued--
		out = append(out, v)
	}
	return out
}

// ReleasePeer permits bounded adapters to expire inactive peer identities.
func (m *Model) ReleasePeer(name string) bool {
	if p := m.peers[name]; p != nil && p.queued != 0 {
		return false
	}
	delete(m.peers, name)
	return true
}

func (m *Model) Close() {
	clear(m.queue)
	m.queue = nil
	clear(m.peers)
	m.counts.QueuedBytes = 0
	m.closed = true
}
