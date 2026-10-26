package shaping

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

type wireConn struct {
	bytes.Buffer
	writes []int
	closed bool
}

func (c *wireConn) Write(p []byte) (int, error) {
	c.writes = append(c.writes, len(p))
	return c.Buffer.Write(p)
}
func (c *wireConn) Close() error                     { c.closed = true; return nil }
func (c *wireConn) LocalAddr() net.Addr              { return nil }
func (c *wireConn) RemoteAddr() net.Addr             { return nil }
func (c *wireConn) SetDeadline(time.Time) error      { return nil }
func (c *wireConn) SetReadDeadline(time.Time) error  { return nil }
func (c *wireConn) SetWriteDeadline(time.Time) error { return nil }

func TestRoundTripBudgetAndStop(t *testing.T) {
	for _, size := range []int{1, 7, 1024, 32768, 1024 * 1024} {
		original := bytes.Repeat([]byte{0x53}, size)
		wire := &wireConn{}
		writer := NewClient(wire)
		if n, err := writer.Write(original); n != size || err != nil {
			t.Fatal(n, err)
		}
		if wire.Len()-size > SendBudget {
			t.Fatal("padding exceeded budget")
		}
		before := wire.Len()
		if n, err := writer.Write(nil); n != 0 || err != nil || wire.Len() != before {
			t.Fatal("empty write emitted traffic")
		}
		decoded, err := io.ReadAll(NewClient(wire))
		if err != nil || !bytes.Equal(decoded, original) {
			t.Fatal("corrupted payload", size, err)
		}
		if size > Records*MaxData {
			if len(wire.writes) != Records+1 {
				t.Fatal("did not stop framing", wire.writes)
			}
			for _, n := range wire.writes[:Records] {
				if n > 8+MaxData+SendPadding {
					t.Fatal("oversized early write")
				}
			}
		}
	}
}

func TestRejectMalformedFrames(t *testing.T) {
	for _, frame := range [][2]uint16{{0, 0}, {MaxData + 1, 0}, {1, MaxPadding + 1}} {
		wire := &wireConn{}
		var hdr [4]byte
		binary.BigEndian.PutUint16(hdr[:2], frame[0])
		binary.BigEndian.PutUint16(hdr[2:], frame[1])
		wire.Buffer.Write(preface[:])
		wire.Buffer.Write(hdr[:])
		if _, err := NewClient(wire).Read(make([]byte, 4)); err == nil || !wire.closed {
			t.Fatal("accepted unbounded frame", frame)
		}
	}
}

func TestBidirectionalAndCancellation(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	a.SetDeadline(time.Now().Add(3 * time.Second))
	b.SetDeadline(time.Now().Add(3 * time.Second))
	left, right := NewClient(a), NewClient(b)
	want := bytes.Repeat([]byte{42}, 32768)
	var group sync.WaitGroup
	for _, pair := range [][2]net.Conn{{left, right}, {right, left}} {
		group.Add(2)
		go func(w net.Conn) {
			defer group.Done()
			if _, err := w.Write(want); err != nil {
				t.Error(err)
			}
		}(pair[0])
		go func(r net.Conn) {
			defer group.Done()
			got := make([]byte, len(want))
			if _, err := io.ReadFull(r, got); err != nil || !bytes.Equal(got, want) {
				t.Error("roundtrip", err)
			}
		}(pair[1])
	}
	group.Wait()
	done := make(chan error, 1)
	go func() { _, err := left.Read(make([]byte, 1)); done <- err }()
	left.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("closed read succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("close did not cancel read")
	}
}

func BenchmarkEarlyPadding(b *testing.B) {
	payload := bytes.Repeat([]byte{42}, 32768)
	var wire wireConn
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	for i := 0; i < b.N; i++ {
		wire.Reset()
		wire.writes = wire.writes[:0]
		if _, err := NewClient(&wire).Write(payload); err != nil {
			b.Fatal(err)
		}
	}
}

type interruptedConn struct {
	*wireConn
	until       int
	interrupted bool
}
type readTimeout struct{}

func (readTimeout) Error() string   { return "injected temporary deadline" }
func (readTimeout) Timeout() bool   { return true }
func (readTimeout) Temporary() bool { return true }
func (c *interruptedConn) Read(p []byte) (int, error) {
	if !c.interrupted {
		if c.until == 0 {
			c.interrupted = true
			return 0, readTimeout{}
		}
		p = p[:min(len(p), c.until)]
		n, err := c.wireConn.Read(p)
		c.until -= n
		return n, err
	}
	return c.wireConn.Read(p)
}

func TestTemporaryDeadlinePreservesEveryEarlyByte(t *testing.T) {
	want := bytes.Repeat([]byte{0x65}, Records*MaxData+1)
	wire := &wireConn{}
	if _, err := NewClient(wire).Write(want); err != nil {
		t.Fatal(err)
	}
	encoded := append([]byte(nil), wire.Bytes()...)
	// Interrupt at every byte, including the version prefix, frame header,
	// payload, padding and transition to passthrough. Retry on the same stream.
	for cut := 0; cut < len(encoded); cut++ {
		input := &interruptedConn{wireConn: &wireConn{}, until: cut}
		input.Buffer.Write(encoded)
		conn := NewClient(input)
		var got []byte
		buffer := make([]byte, 257)
		for len(got) < len(want) {
			n, err := conn.Read(buffer)
			got = append(got, buffer[:n]...)
			if err != nil {
				if _, ok := err.(readTimeout); !ok {
					t.Fatal(cut, err)
				}
			}
			if input.closed {
				t.Fatal("temporary read deadline closed connection", cut)
			}
		}
		if !bytes.Equal(got, want) {
			t.Fatal("deadline lost bytes", cut)
		}
	}
}

func TestReceiverEnforcesTotalBudget(t *testing.T) {
	wire := &wireConn{}
	wire.Buffer.Write(preface[:])
	for i := 0; i < Records; i++ {
		wire.Buffer.Write([]byte{0, 1, 1, 0}) // 256 padding bytes per frame: over budget.
		wire.Buffer.Write(make([]byte, 257))
	}
	if _, err := io.ReadAll(NewClient(wire)); err == nil || !wire.closed {
		t.Fatal("peer exceeded total budget")
	}
}

func TestReceivesEarlierV1PaddingBudget(t *testing.T) {
	wire := &wireConn{}
	wire.Buffer.Write(preface[:])
	for i := 0; i < Records; i++ {
		wire.Buffer.Write([]byte{0, 1, 0, 100})
		wire.Buffer.WriteByte('x')
		wire.Buffer.Write(make([]byte, 100))
	}
	if wire.Len()-Records <= SendBudget {
		t.Fatal("fixture did not exceed current send budget")
	}
	got, err := io.ReadAll(NewClient(wire))
	if err != nil || string(got) != "xxxxxxxx" {
		t.Fatal("broke bounded-v1 compatibility", string(got), err)
	}
}

type partialWriter struct{ *wireConn }

func (c partialWriter) Write(p []byte) (int, error) {
	c.Buffer.Write(p[:min(10, len(p))])
	return min(10, len(p)), io.ErrUnexpectedEOF
}
func TestPartialFrameWriteClosesWithoutReplay(t *testing.T) {
	wire := &wireConn{}
	n, err := NewClient(partialWriter{wire}).Write(bytes.Repeat([]byte{3}, 100))
	if err == nil || !wire.closed || n != 2 || wire.Len() != 10 {
		t.Fatal(n, err, wire.Len(), wire.closed)
	}
}
