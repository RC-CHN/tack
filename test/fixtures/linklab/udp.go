package linklab

import (
	"container/heap"
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

type UDPStats struct {
	Model                    Counts
	Peers                    int
	InputDrops, SocketErrors uint64
}

type incoming struct {
	source    string
	address   *net.UDPAddr
	bytes     []byte
	direction Direction
	owner     *udpPeer
}
type udpPeer struct {
	conn    *net.UDPConn
	address *net.UDPAddr
	last    time.Time
}
type lossChange struct {
	direction       Direction
	fraction, burst float64
	result          chan error
}

// UDP is a bounded live adapter around Model. One scheduler owns all modeled
// state. There is one socket reader per admitted peer, not per datagram.
type UDP struct {
	listener      *net.UDPConn
	target        *net.UDPAddr
	model         *Model
	idle          time.Duration
	ctx           context.Context
	cancel        context.CancelFunc
	incoming      chan incoming
	changes       chan lossChange
	wg            sync.WaitGroup
	once          sync.Once
	mu            sync.Mutex
	stats         UDPStats
	drops, errors atomic.Uint64
}

func NewUDP(target string, c Config, idle time.Duration) (*UDP, error) {
	m, err := New(c)
	if err != nil {
		return nil, err
	}
	if idle <= 0 || idle > time.Hour {
		return nil, errors.New("UDP peer expiry must be within one hour")
	}
	address, err := net.ResolveUDPAddr("udp", target)
	if err != nil {
		return nil, err
	}
	listener, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	u := &UDP{listener: listener, target: address, model: m, idle: idle, ctx: ctx, cancel: cancel, incoming: make(chan incoming, 16), changes: make(chan lossChange)}
	u.wg.Add(2)
	go u.read(listener, "", nil)
	go u.run()
	return u, nil
}

func (u *UDP) Addr() string { return u.listener.LocalAddr().String() }
func (u *UDP) Snapshot() UDPStats {
	u.mu.Lock()
	v := u.stats
	u.mu.Unlock()
	v.InputDrops = u.drops.Load()
	v.SocketErrors = u.errors.Load()
	return v
}
func (u *UDP) SetLoss(d Direction, fraction, burst float64) error {
	r := lossChange{d, fraction, burst, make(chan error, 1)}
	select {
	case u.changes <- r:
	case <-u.ctx.Done():
		return net.ErrClosed
	}
	select {
	case err := <-r.result:
		return err
	case <-u.ctx.Done():
		return net.ErrClosed
	}
}
func (u *UDP) Close() error {
	u.once.Do(func() {
		u.cancel()
		u.listener.Close()
		u.wg.Wait()
		for {
			select {
			case <-u.incoming:
			default:
				return
			}
		}
	})
	return nil
}

func (u *UDP) read(conn *net.UDPConn, source string, owner *udpPeer) {
	defer u.wg.Done()
	buf := make([]byte, 65535)
	for {
		n, address, err := conn.ReadFromUDP(buf)
		if err != nil {
			if u.ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			u.errors.Add(1)
			if datagramError(err) {
				continue
			}
			return
		}
		v := incoming{source: source, address: address, bytes: append([]byte(nil), buf[:n]...), direction: Download, owner: owner}
		if owner == nil {
			v.source = address.String()
			v.direction = Upload
		}
		select {
		case <-u.ctx.Done():
			return
		case u.incoming <- v:
		default:
			u.drops.Add(1)
		}
	}
}

func (m *Model) discardPeer(name string) {
	kept := m.queue[:0]
	for _, v := range m.queue {
		if v.Peer == name {
			m.counts.QueuedBytes -= max(1, len(v.Data))
			m.counts.Lost++
		} else {
			kept = append(kept, v)
		}
	}
	clear(m.queue[len(kept):])
	m.queue = kept
	heap.Init(&m.queue)
	delete(m.peers, name)
}

func (u *UDP) run() {
	defer u.wg.Done()
	peers := make(map[string]*udpPeer)
	defer func() {
		for _, p := range peers {
			p.conn.Close()
		}
		u.model.Close()
		u.mu.Lock()
		u.stats.Model = u.model.Snapshot()
		u.stats.Peers = 0
		u.mu.Unlock()
	}()
	start := time.Now()
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for {
		now := time.Now()
		for _, v := range u.model.Drain(now.Sub(start)) {
			p := peers[v.Peer]
			if p == nil {
				continue
			}
			var err error
			if v.Direction == Upload {
				p.conn.SetWriteDeadline(now.Add(100 * time.Millisecond))
				_, err = p.conn.Write(v.Data)
			} else {
				u.listener.SetWriteDeadline(now.Add(100 * time.Millisecond))
				_, err = u.listener.WriteToUDP(v.Data, p.address)
			}
			if err != nil {
				u.errors.Add(1)
			}
		}
		for name, p := range peers {
			if now.Sub(p.last) >= u.idle {
				p.conn.Close()
				delete(peers, name)
				u.model.discardPeer(name)
			}
		}
		u.mu.Lock()
		u.stats.Model = u.model.Snapshot()
		u.stats.Peers = len(peers)
		u.mu.Unlock()
		due := now.Add(time.Hour)
		if len(u.model.queue) > 0 {
			due = start.Add(u.model.queue[0].At)
		}
		for _, p := range peers {
			if p.last.Add(u.idle).Before(due) {
				due = p.last.Add(u.idle)
			}
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(max(0, time.Until(due)))
		select {
		case <-u.ctx.Done():
			return
		case change := <-u.changes:
			change.result <- u.model.SetLoss(change.direction, change.fraction, change.burst)
		case <-timer.C:
		case v := <-u.incoming:
			p := peers[v.source]
			if v.direction == Download && (p == nil || p != v.owner) {
				continue
			} // Retired socket cannot target a new peer generation.
			if p == nil {
				if len(peers) >= u.model.config.Peers {
					u.drops.Add(1)
					continue
				}
				conn, err := net.DialUDP("udp", nil, u.target)
				if err != nil {
					u.errors.Add(1)
					continue
				}
				p = &udpPeer{conn: conn, address: v.address}
				peers[v.source] = p
				u.wg.Add(1)
				go u.read(conn, v.source, p)
			}
			p.last = time.Now()
			_, err := u.model.Submit(p.last.Sub(start), v.source, v.direction, v.bytes)
			if err != nil {
				u.errors.Add(1)
			}
		}
	}
}
