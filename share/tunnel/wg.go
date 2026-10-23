package tunnel

import "sync"

type waitGroup struct {
	mu   sync.Mutex
	done chan struct{}
	n    int
}

func (w *waitGroup) Add(n int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.n+n < 0 {
		panic("negative waitGroup counter")
	}
	if w.n == 0 && n > 0 {
		w.done = make(chan struct{})
	}
	w.n += n
	if w.n == 0 && n < 0 {
		close(w.done)
	}
}

func (w *waitGroup) Done() {
	w.mu.Lock()
	if w.n > 0 {
		w.n--
		if w.n == 0 {
			close(w.done)
		}
	}
	w.mu.Unlock()
}

func (w *waitGroup) DoneAll() {
	w.mu.Lock()
	if w.n > 0 {
		w.n = 0
		close(w.done)
	}
	w.mu.Unlock()
}

func (w *waitGroup) Wait() {
	<-w.WaitChan()
}

// WaitChan allows callers to select on cancellation without leaving a helper
// goroutine waiting until the next connection. Each nonzero generation gets
// its own channel, so a subsequent Add cannot re-block already released waits.
func (w *waitGroup) WaitChan() <-chan struct{} {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.done == nil {
		w.done = make(chan struct{})
		close(w.done) // The zero-value group is already complete.
	}
	return w.done
}
