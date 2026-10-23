package tunnel

import (
	"sync"
	"testing"
	"time"
)

// TestWaitGroupAddDoneAllRace stresses concurrent Add/DoneAll to reproduce the
// race fixed in #585. Without the mutex, Add() is non-atomic between bumping
// the counter and the inner sync.WaitGroup, so DoneAll()'s loop can call
// inner.Done() before inner.Add() runs and panic with "negative WaitGroup
// counter".
func TestWaitGroupAddDoneAllRace(t *testing.T) {
	var wg waitGroup
	stop := make(chan struct{})
	var workers sync.WaitGroup
	for i := 0; i < 8; i++ {
		workers.Add(2)
		go func() {
			defer workers.Done()
			for {
				select {
				case <-stop:
					return
				default:
					wg.Add(1)
				}
			}
		}()
		go func() {
			defer workers.Done()
			for {
				select {
				case <-stop:
					return
				default:
					wg.DoneAll()
				}
			}
		}()
	}
	time.Sleep(500 * time.Millisecond)
	close(stop)
	workers.Wait()
	wg.DoneAll()
	wg.Wait()
}

func TestWaitGroupDoneAllDrains(t *testing.T) {
	var wg waitGroup
	wg.Add(3)
	wg.DoneAll()
	wg.Wait()
}

func TestWaitGroupDoneIsIdempotent(t *testing.T) {
	var wg waitGroup
	wg.Add(1)
	wg.Done()
	wg.Done() // extra Done must be a no-op, not a panic
	wg.Wait()
}

func TestWaitGroupGenerationWakeups(t *testing.T) {
	var wg waitGroup
	select {
	case <-wg.WaitChan():
	default:
		t.Fatal("zero-value group must be ready")
	}
	for i := 0; i < 100; i++ {
		wg.Add(2)
		previous := wg.WaitChan()
		wg.Done()
		select {
		case <-previous:
			t.Fatal("released before the group completed")
		default:
		}
		wg.DoneAll()
		wg.Add(1)
		select {
		case <-previous:
		default:
			t.Fatal("subsequent Add re-blocked an already completed generation")
		}
		select {
		case <-wg.WaitChan():
			t.Fatal("new generation is already complete")
		default:
		}
		wg.Done()
	}
}
