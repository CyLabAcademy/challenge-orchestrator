package cmgr

import (
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
)

func testWorkerConn(h workerHealth) *workerConn {
	w := &workerConn{ip: "10.0.0.1"}
	w.health.Store(int32(h))
	return w
}

func healthOf(w *workerConn) workerHealth { return workerHealth(w.health.Load()) }

// The poller may move a worker between ok and overloaded but must never take
// it out of down, which only worker-add clears.
func TestPollerSetHealthNeverLeavesDown(t *testing.T) {
	m := &Manager{log: newLogger(DISABLED)}
	w := testWorkerConn(workerOk)

	m.pollerSetHealth(w, workerOverloaded)
	if got := healthOf(w); got != workerOverloaded {
		t.Fatalf("after an overloaded verdict: %s", got)
	}
	m.pollerSetHealth(w, workerOk)
	if got := healthOf(w); got != workerOk {
		t.Fatalf("after an ok verdict: %s", got)
	}

	m.markWorkerDown(w) // SetWorkerDown or a transport error
	for _, verdict := range []workerHealth{workerOk, workerOverloaded} {
		m.pollerSetHealth(w, verdict)
		if got := healthOf(w); got != workerDown {
			t.Fatalf("poller verdict %s took the worker out of down: %s", verdict, got)
		}
	}
}

// A worker-down that lands while polls are in flight stays down. With the
// old swap store, a poll that started before the worker-down could land
// after it and flip the worker back to ok (seen by the e2e all-down step).
func TestPollerSetHealthRacesWorkerDown(t *testing.T) {
	m := &Manager{log: newLogger(DISABLED)}
	w := testWorkerConn(workerOk)

	stop := make(chan struct{})
	var stores atomic.Int64
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if i%2 == 0 {
				m.pollerSetHealth(w, workerOk)
			} else {
				m.pollerSetHealth(w, workerOverloaded)
			}
			stores.Add(1)
		}
	}()

	// Flip the worker down only once verdicts are really flowing, and keep
	// them flowing for a while afterwards.
	for stores.Load() < 100 {
		runtime.Gosched()
	}
	m.markWorkerDown(w)
	for target := stores.Load() + 1000; stores.Load() < target; {
		runtime.Gosched()
	}
	close(stop)
	wg.Wait()

	if got := healthOf(w); got != workerDown {
		t.Fatalf("concurrent poll verdicts took the worker out of down: %s", got)
	}
}
