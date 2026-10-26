// Package shaping implements Tack's bounded-v1 stream inside verified TLS.
// It is not AnyTLS and provides no encryption or authentication of its own.
package shaping

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
)

const (
	Profile     = "bounded-v1"
	Records     = 8
	Budget      = 1024 // Wire receive ceiling; accepts earlier bounded-v1 peers.
	MaxData     = 1024
	MaxPadding  = 256
	SendBudget  = 256 // Prefix, headers and padding emitted by this implementation.
	SendPadding = 64
)

var preface = [4]byte{0, 'T', 1, 0}

func Enabled(name string) bool { return name == Profile }
func Check(name string) error {
	if name == "" || name == "off" || Enabled(name) {
		return nil
	}
	return errors.New("traffic shaping must be off or bounded-v1")
}

// Conn has no background worker, timer, queue, or idle writes. After eight
// nonempty frames in each direction it becomes a transparent byte stream.
// The enclosing TLS connection must be verified before NewClient is called.
type Conn struct {
	net.Conn
	rmu, wmu           sync.Mutex
	rframes, wframes   int
	rbudget, wbudget   int
	remaining, discard int
	rprefix, wprefix   bool
	rheader            [4]byte
	rheaderN           int
	writeBuffer        [8 + MaxData + SendPadding]byte
	scratch            [MaxPadding]byte
}

// NewClient requires the peer to confirm exactly bounded-v1 in its first
// encrypted response. A plain HTTPS response fails; there is no downgrade.
func NewClient(conn net.Conn) *Conn {
	return &Conn{Conn: conn, rbudget: Budget - 4, wbudget: SendBudget - 4, rprefix: true, wprefix: true}
}

func (c *Conn) Write(p []byte) (int, error) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	written := 0
	for len(p) > 0 && c.wframes < Records {
		var random [4]byte
		if _, err := rand.Read(random[:]); err != nil {
			c.Close()
			return written, err
		}
		// Short headers stay in one record; larger early writes still vary in
		// length without a train of tiny records on a narrow intermittent link.
		n := min(len(p), 512+int(binary.BigEndian.Uint16(random[:2]))%(MaxData-511))
		available := min(SendPadding, c.wbudget-4*(Records-c.wframes))
		pad := int(binary.BigEndian.Uint16(random[2:])) % (available + 1)
		prefix := 0
		if c.wprefix {
			prefix = len(preface)
			copy(c.writeBuffer[:], preface[:])
		}
		packet := c.writeBuffer[:prefix+4+n+pad]
		binary.BigEndian.PutUint16(packet[prefix:prefix+2], uint16(n))
		binary.BigEndian.PutUint16(packet[prefix+2:prefix+4], uint16(pad))
		copy(packet[prefix+4:], p[:n])
		clear(packet[prefix+4+n:]) // Encrypted by TLS, never written to raw TCP.
		count, err := writeAll(c.Conn, packet)
		if err != nil {
			c.Close()
			return written + max(0, min(n, count-prefix-4)), err
		}
		c.wprefix = false
		written += n
		p = p[n:]
		c.wframes++
		c.wbudget -= 4 + pad
	}
	if len(p) > 0 {
		n, err := c.Conn.Write(p)
		return written + n, err
	}
	return written, nil
}

func (c *Conn) Read(p []byte) (n int, err error) {
	if len(p) == 0 {
		return 0, nil
	}
	c.rmu.Lock()
	defer c.rmu.Unlock()
	if c.rprefix {
		if err = c.readHeader(); err != nil {
			return 0, err
		}
		if c.rheader != preface {
			c.Close()
			return 0, errors.New("peer did not confirm bounded-v1 shaping")
		}
		c.rheaderN = 0
		c.rprefix = false
	}
	if c.remaining > 0 {
		n, err = c.Conn.Read(p[:min(len(p), c.remaining)])
		c.remaining -= n
		return
	}
	for c.discard > 0 {
		var consumed int
		consumed, err = c.Conn.Read(c.scratch[:c.discard])
		c.discard -= consumed
		if err != nil {
			return 0, err
		}
		if consumed == 0 {
			return 0, io.ErrNoProgress
		}
	}
	if c.rframes == Records {
		return c.Conn.Read(p)
	}
	if err = c.readHeader(); err != nil {
		return 0, err
	}
	data, pad := int(binary.BigEndian.Uint16(c.rheader[:2])), int(binary.BigEndian.Uint16(c.rheader[2:]))
	if data == 0 || data > MaxData || pad > MaxPadding || 4+pad > c.rbudget-4*(Records-c.rframes-1) {
		c.Close()
		return 0, errors.New("invalid bounded-v1 frame")
	}
	c.rheaderN = 0
	c.rframes++
	c.rbudget -= 4 + pad
	c.remaining, c.discard = data, pad
	n, err = c.Conn.Read(p[:min(len(p), data)])
	c.remaining -= n
	return
}

// net/http interrupts its background reads with temporary deadlines between
// requests. Preserve partial framing state, so cancelling a read never closes
// a healthy keep-alive connection or loses bytes already consumed from TLS.
func (c *Conn) readHeader() error {
	for c.rheaderN < len(c.rheader) {
		n, err := c.Conn.Read(c.rheader[c.rheaderN:])
		c.rheaderN += n
		if c.rheaderN == len(c.rheader) {
			return nil
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrNoProgress
		}
	}
	return nil
}

func writeAll(w io.Writer, p []byte) (int, error) {
	total := 0
	for len(p) > 0 {
		n, err := w.Write(p)
		total += n
		p = p[n:]
		if err != nil {
			return total, err
		}
		if n == 0 {
			return total, io.ErrShortWrite
		}
	}
	return total, nil
}
