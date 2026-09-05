package cmgr

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/moby/moby/client"
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

func TestWorkerTimingDefaults(t *testing.T) {
	m := &Manager{log: newLogger(DISABLED)}
	if got := m.workerTimingFromEnv(); got != defaultWorkerTiming {
		t.Fatalf("no overrides set, got %+v", got)
	}
	// A Manager not built by NewManager still gets real timeouts for its
	// control-plane calls.
	if got := (&Manager{}).timing(); got != defaultWorkerTiming {
		t.Fatalf("zero-value fallback: %+v", got)
	}
}

func TestWorkerTimingFromEnv(t *testing.T) {
	m := &Manager{log: newLogger(DISABLED)}
	t.Setenv(WORKER_POLL_INTERVAL_ENV, "100ms")
	t.Setenv(WORKER_POLL_TIMEOUT_ENV, "40ms")
	t.Setenv(WORKER_MAX_MISSES_ENV, "5")
	t.Setenv(WORKER_CONTROL_TIMEOUT_ENV, "3s")
	t.Setenv(WORKER_PULL_TIMEOUT_ENV, "1m")
	want := workerTiming{
		pollInterval:   100 * time.Millisecond,
		pollTimeout:    40 * time.Millisecond,
		maxMisses:      5,
		controlTimeout: 3 * time.Second,
		pullTimeout:    time.Minute,
	}
	if got := m.workerTimingFromEnv(); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestWorkerTimingFromEnvIgnoresBadValues(t *testing.T) {
	m := &Manager{log: newLogger(DISABLED)}
	t.Setenv(WORKER_POLL_INTERVAL_ENV, "soon")
	t.Setenv(WORKER_MAX_MISSES_ENV, "0")
	t.Setenv(WORKER_CONTROL_TIMEOUT_ENV, "-1s")
	t.Setenv(WORKER_PULL_TIMEOUT_ENV, "0")
	if got := m.workerTimingFromEnv(); got != defaultWorkerTiming {
		t.Fatalf("bad overrides were not ignored: %+v", got)
	}
}

func TestWorkerTimingClampsPollTimeout(t *testing.T) {
	m := &Manager{log: newLogger(DISABLED)}
	// Shortening only the interval leaves the default timeout too long.
	t.Setenv(WORKER_POLL_INTERVAL_ENV, "100ms")
	if got := m.workerTimingFromEnv(); got.pollTimeout != 50*time.Millisecond {
		t.Fatalf("poll timeout not clamped to half the interval: %s", got.pollTimeout)
	}
	t.Setenv(WORKER_POLL_TIMEOUT_ENV, "100ms")
	if got := m.workerTimingFromEnv(); got.pollTimeout != 50*time.Millisecond {
		t.Fatalf("poll timeout equal to the interval not clamped: %s", got.pollTimeout)
	}
}

// The docker client reports a refused connection with an error of its own that
// wraps a message, not the net.Error. It must still count as a transport
// failure, or a daemon that is down without hanging would never be noticed:
// not marked down, not retried at reconcile, its launches answered 500. A
// Unix socket that does not exist is classified by the client exactly like a
// refused TCP connection and fails at once on every platform, where a closed
// loopback port can hang behind a forwarder.
func TestIsTransportErrorRefusedConnection(t *testing.T) {
	cli, err := client.NewClientWithOpts(client.WithHost("unix://" + filepath.Join(t.TempDir(), "no-daemon.sock")))
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	defer cli.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = cli.Ping(ctx, client.PingOptions{})
	if err == nil {
		t.Fatal("a ping to a missing socket succeeded")
	}
	if !client.IsErrConnectionFailed(err) {
		t.Fatalf("the client did not classify the failure as a connection failure: %v", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the dial timed out instead of failing at once: %v", err)
	}
	if !isTransportError(err) {
		t.Fatalf("a refused connection is not a transport error: %v", err)
	}
	if isTransportError(errors.New("network with name cmgr-7 already exists")) {
		t.Fatal("a plain daemon error counted as a transport error")
	}
}
