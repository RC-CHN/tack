package runner

import "sync"

// Keep the beginning and a circular tail, sharing one fixed byte budget. Output
// is binary: no text marker or encoding conversion is inserted into the bytes.
// The protocol's truncated flag tells readers that the middle was omitted.
type bounded struct {
	mu               sync.Mutex
	data             []byte
	limit            int
	head, tail, next int
	truncated        bool
}

func (b *bounded) Write(v []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(v)
	if n == 0 {
		return 0, nil
	}
	if b.limit <= 0 {
		b.truncated = true
		return n, nil
	}
	if n > b.limit-b.head-b.tail {
		b.truncated = true
	}
	needed := b.limit
	if n < b.limit-b.head-b.tail {
		needed = b.head + b.tail + n
	}
	if len(b.data) < needed {
		capacity := len(b.data) * 2
		if capacity < 64 {
			capacity = 64
		}
		if capacity < needed {
			capacity = needed
		}
		if capacity > b.limit {
			capacity = b.limit
		}
		data := make([]byte, capacity)
		copy(data, b.data)
		b.data = data
	}
	headCap := (b.limit + 1) / 2
	if b.head < headCap {
		end := headCap
		if end > len(b.data) {
			end = len(b.data)
		}
		m := copy(b.data[b.head:end], v)
		b.head += m
		v = v[m:]
	}
	tailCap := b.limit - headCap
	if tailCap == 0 || len(v) == 0 {
		return n, nil
	}
	if len(v) >= tailCap {
		copy(b.data[headCap:], v[len(v)-tailCap:])
		b.tail = tailCap
		b.next = 0
	} else {
		m := copy(b.data[headCap+b.next:], v)
		copy(b.data[headCap:], v[m:])
		b.next = (b.next + len(v)) % tailCap
		b.tail += len(v)
		if b.tail > tailCap {
			b.tail = tailCap
		}
	}
	return n, nil
}

func (b *bounded) snapshot() ([]byte, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]byte, 0, b.head+b.tail)
	out = append(out, b.data[:b.head]...)
	headCap := (b.limit + 1) / 2
	if b.tail > 0 {
		if b.tail == b.limit-headCap {
			out = append(out, b.data[headCap+b.next:]...)
			out = append(out, b.data[headCap:headCap+b.next]...)
		} else {
			out = append(out, b.data[headCap:headCap+b.tail]...)
		}
	}
	return out, b.truncated
}
