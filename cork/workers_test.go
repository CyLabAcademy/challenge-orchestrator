package cork

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

// testWorkerConn builds a conn in the given state. It carries a real, if
// useless, docker client: recoverWorker closes the conn it replaces, and a nil
// client there is a nil-pointer dereference in a goroutine no test can
// recover, which takes the whole package's binary with it. Nothing production
// builds ever has a nil cli (newWorkerConn always sets one), so a fixture that
// does is a trap rather than a simplification. The address is one nothing
// listens on, so any call it is asked to make fails at once.
func testWorkerConn(r workerReachable) *workerConn {
	cli, err := client.NewClientWithOpts(client.WithHost("tcp://127.0.0.1:1"), client.WithVersion("1.44"))
	if err != nil {
		panic("building the test docker client: " + err.Error())
	}
	w := &workerConn{ip: "10.0.0.1", cli: cli, unreachCh: make(chan struct{}), probeNow: make(chan struct{}, 1)}
	w.reachable.Store(int32(r))
	w.load.Store(int32(workerLoadOk))
	return w
}

func reachableOf(w *workerConn) workerReachable { return workerReachable(w.reachable.Load()) }
func loadOf(w *workerConn) workerLoad           { return workerLoad(w.load.Load()) }

// Down is asserted by an operator, who knows something the probes do not, so
// no observation may lift it -- however healthy the daemon looks afterwards.
// This is the whole difference between it and the unresponsive a failed probe
// produces.
func TestAssertedDownIgnoresObservations(t *testing.T) {
	m := &Manager{log: newLogger(DISABLED)}
	w := testWorkerConn(workerReachableOk)
	m.markWorkerDown(w)

	for _, to := range []workerReachable{workerReachableOk, workerUnresponsive} {
		if m.setReachable(w, to, "probe says otherwise") {
			t.Fatalf("an observation moved a worker out of asserted down to %s", to)
		}
		if got := reachableOf(w); got != workerDown {
			t.Fatalf("worker left asserted down: %s", got)
		}
	}
}

// A conn is one-shot. Leaving ok wakes everything queued on that daemon by
// closing unreachCh, and a closed channel stays ready forever -- so a conn
// that was allowed back into placement would hand every launch selecting on
// it an instant failure for as long as it lived, and a second departure would
// close the channel twice. Recovery replaces the conn instead, which is what
// gives a recovered worker a channel that is open again.
func TestAConnNeverReturnsToOk(t *testing.T) {
	m := &Manager{log: newLogger(DISABLED)}
	w := testWorkerConn(workerReachableOk)

	select {
	case <-w.unreachCh:
		t.Fatal("waiters were woken while the worker was still reachable")
	default:
	}

	m.eject(w, "ping refused")
	select {
	case <-w.unreachCh:
	default:
		t.Fatal("ejecting a worker did not wake its waiters")
	}
	if got := w.ejections.Load(); got != 1 {
		t.Fatalf("ejections %d, want 1", got)
	}

	if m.setReachable(w, workerReachableOk, "answering again") {
		t.Fatal("a retired conn was put back into placement instead of being replaced")
	}
	if got := reachableOf(w); got != workerUnresponsive {
		t.Fatalf("reachable %s after a refused re-admission, want unresponsive", got)
	}
	// And nothing here closes unreachCh a second time.
	m.eject(w, "ping refused again")
	m.markWorkerDown(w)
}

// The reconcile that admits a worker must not do so behind the back of a
// control call that failed while it ran: the conn is retired by then, and the
// poller replaces it rather than admitting a daemon nothing has re-proved.
func TestAdmissionLosesToAConcurrentEjection(t *testing.T) {
	m := &Manager{log: newLogger(DISABLED)}
	w := testWorkerConn(workerReachableOk)

	m.eject(w, "a control call failed while the reconcile ran")
	if m.setReachable(w, workerReachableOk, "") {
		t.Fatal("a reconcile admitted a worker that had just been ejected under it")
	}
	if got := reachableOf(w); got != workerUnresponsive {
		t.Fatalf("reachable %s, want the ejection to stand", got)
	}
}

// Load moves independently of reachability, and never takes a worker out of
// the fleet by itself: a box whose telemetry agent died is very often serving
// perfectly well.
func TestLoadIsIndependentOfReachability(t *testing.T) {
	m := &Manager{log: newLogger(DISABLED)}
	w := testWorkerConn(workerReachableOk)

	m.setLoad(w, workerLoadUnknown)
	if got := reachableOf(w); got != workerReachableOk {
		t.Fatalf("telemetry going silent changed reachability to %s", got)
	}
	if !placeable(w) {
		t.Fatal("a worker with unknown load was refused placement")
	}

	m.setLoad(w, workerOverloaded)
	if placeable(w) {
		t.Fatal("an overloaded worker was offered a placement")
	}
	if got := reachableOf(w); got != workerReachableOk {
		t.Fatalf("an overloaded worker was treated as unreachable: %s", got)
	}
}

// A control call that fails while a worker's first reconcile pass is still
// running says nothing about the box: the pass is deliberately waiting out a
// daemon that is still starting. Ejecting it there would restart the recovery
// backoff on a worker that has not failed at anything yet, so the verdict is
// left to the pass.
func TestTransportErrorSparesAnUnreconciledWorker(t *testing.T) {
	cli, err := client.NewClientWithOpts(client.WithHost("unix://" + filepath.Join(t.TempDir(), "no-daemon.sock")))
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	defer cli.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, refused := cli.Ping(ctx, client.PingOptions{})
	if refused == nil {
		t.Fatal("a ping to a missing socket succeeded")
	}

	m := &Manager{log: newLogger(DISABLED)}
	w := testWorkerConn(workerReachableOk)
	m.workers = map[string]*workerConn{w.ip: w}

	m.noteWorkerTransportError(w.ip, w.cli, refused)
	if got := reachableOf(w); got != workerReachableOk {
		t.Fatalf("a transport error ejected a worker whose first reconcile had not finished: %s", got)
	}

	w.reconciled.Store(true)
	m.noteWorkerTransportError(w.ip, w.cli, refused)
	if got := reachableOf(w); got != workerUnresponsive {
		t.Fatalf("reachable %s after a transport error on a reconciled worker, want unresponsive", got)
	}
	// And the poller is asked to look now rather than at its next tick, so a
	// daemon that is merely restarting is back in seconds.
	select {
	case <-w.probeNow:
	default:
		t.Fatal("a failed control call did not wake the probe")
	}
}

// A worker's connection is rebuilt whenever it comes back: by worker-add, and
// by the probe recovering it. A call that was already in flight on the old one
// can fail long afterwards -- a hung call runs to the transport timeout -- and
// must not eject the box that is answering now. This is why recovery replaces
// the conn instead of clearing its flags.
func TestTransportErrorSparesAReplacedConnection(t *testing.T) {
	socket := "unix://" + filepath.Join(t.TempDir(), "no-daemon.sock")
	stale, err := client.NewClientWithOpts(client.WithHost(socket))
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	defer stale.Close()
	current, err := client.NewClientWithOpts(client.WithHost(socket))
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	defer current.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, refused := stale.Ping(ctx, client.PingOptions{})
	if refused == nil {
		t.Fatal("a ping to a missing socket succeeded")
	}

	m := &Manager{log: newLogger(DISABLED)}
	w := testWorkerConn(workerReachableOk)
	w.reconciled.Store(true)
	w.cli = current
	m.workers = map[string]*workerConn{w.ip: w}

	m.noteWorkerTransportError(w.ip, stale, refused)
	if got := reachableOf(w); got != workerReachableOk {
		t.Fatalf("a failure on a replaced connection ejected the worker that replaced it: %s", got)
	}

	m.noteWorkerTransportError(w.ip, current, refused)
	if got := reachableOf(w); got != workerUnresponsive {
		t.Fatalf("reachable %s after a transport error on the live connection, want unresponsive", got)
	}
}

// A worker-down that lands while probes are in flight stays down. The load
// axis keeps moving underneath it -- telemetry has no idea an operator did
// anything -- and must not drag reachability back with it, which is the bug a
// single fused enum had: a verdict that started before the worker-down could
// land after it and flip the worker back to ok (seen by the e2e all-down
// step).
func TestLoadVerdictsRaceWorkerDown(t *testing.T) {
	m := &Manager{log: newLogger(DISABLED)}
	w := testWorkerConn(workerReachableOk)

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
				m.setLoad(w, workerLoadOk)
			} else {
				m.setLoad(w, workerOverloaded)
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

	if got := reachableOf(w); got != workerDown {
		t.Fatalf("concurrent load verdicts took the worker out of down: %s", got)
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
	t.Setenv(WORKER_PING_TIMEOUT_ENV, "60ms")
	t.Setenv(WORKER_DEEP_PROBE_ENV, "1s")
	t.Setenv(WORKER_MAX_MISSES_ENV, "5")
	t.Setenv(WORKER_LOAD_MISSES_ENV, "2")
	t.Setenv(WORKER_HEALTHY_THRESHOLD_ENV, "3")
	t.Setenv(WORKER_RECOVER_BACKOFF_ENV, "4s")
	t.Setenv(WORKER_RECOVER_BACKOFF_MAX_ENV, "9s")
	t.Setenv(WORKER_EJECTION_DECAY_ENV, "7m")
	t.Setenv(WORKER_CONTROL_TIMEOUT_ENV, "3s")
	t.Setenv(WORKER_PULL_TIMEOUT_ENV, "1m")
	t.Setenv(WORKER_LAUNCH_WAIT_ENV, "2s")
	want := workerTiming{
		pollInterval:      100 * time.Millisecond,
		pollTimeout:       40 * time.Millisecond,
		pingTimeout:       60 * time.Millisecond,
		deepProbeInterval: time.Second,
		maxMisses:         5,
		loadMisses:        2,
		healthyThreshold:  3,
		recoverBackoff:    4 * time.Second,
		recoverBackoffMax: 9 * time.Second,
		ejectionDecay:     7 * time.Minute,
		controlTimeout:    3 * time.Second,
		pullTimeout:       time.Minute,
		launchWait:        2 * time.Second,
	}
	if got := m.workerTimingFromEnv(); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestWorkerTimingFromEnvIgnoresBadValues(t *testing.T) {
	m := &Manager{log: newLogger(DISABLED)}
	t.Setenv(WORKER_POLL_INTERVAL_ENV, "soon")
	t.Setenv(WORKER_MAX_MISSES_ENV, "0")
	t.Setenv(WORKER_LOAD_MISSES_ENV, "never")
	t.Setenv(WORKER_HEALTHY_THRESHOLD_ENV, "-2")
	t.Setenv(WORKER_CONTROL_TIMEOUT_ENV, "-1s")
	t.Setenv(WORKER_PULL_TIMEOUT_ENV, "0")
	if got := m.workerTimingFromEnv(); got != defaultWorkerTiming {
		t.Fatalf("bad overrides were not ignored: %+v", got)
	}
}

// An interval below the floor is raised to it, so the timeout derived from
// it stays positive: a zero timeout would mean no timeout at all, and one
// hung telemetry request would stall the poller for good.
func TestWorkerTimingFloorsPollInterval(t *testing.T) {
	m := &Manager{log: newLogger(DISABLED)}
	t.Setenv(WORKER_POLL_INTERVAL_ENV, "1ns")
	got := m.workerTimingFromEnv()
	if got.pollInterval != minPollInterval {
		t.Fatalf("poll interval %s, want the %s floor", got.pollInterval, minPollInterval)
	}
	if got.pollTimeout <= 0 {
		t.Fatalf("poll timeout %s: a poll would have no deadline at all", got.pollTimeout)
	}
}

// Both probes run on the one tick, so neither timeout may reach it: a probe
// still running when the next tick comes round would stall the poller.
func TestWorkerTimingClampsProbeTimeouts(t *testing.T) {
	m := &Manager{log: newLogger(DISABLED)}
	// Shortening only the interval leaves both default timeouts too long.
	t.Setenv(WORKER_POLL_INTERVAL_ENV, "100ms")
	got := m.workerTimingFromEnv()
	if got.pollTimeout != 50*time.Millisecond {
		t.Fatalf("poll timeout not clamped to half the interval: %s", got.pollTimeout)
	}
	if got.pingTimeout != 50*time.Millisecond {
		t.Fatalf("ping timeout not clamped to half the interval: %s", got.pingTimeout)
	}
	t.Setenv(WORKER_POLL_TIMEOUT_ENV, "100ms")
	t.Setenv(WORKER_PING_TIMEOUT_ENV, "100ms")
	got = m.workerTimingFromEnv()
	if got.pollTimeout != 50*time.Millisecond || got.pingTimeout != 50*time.Millisecond {
		t.Fatalf("a timeout equal to the interval was not clamped: poll %s, ping %s", got.pollTimeout, got.pingTimeout)
	}
	// The deep probe rides the same tick, so it cannot be asked to run more
	// often than one.
	if got.deepProbeInterval < got.pollInterval {
		t.Fatalf("deep probe interval %s is shorter than the tick %s", got.deepProbeInterval, got.pollInterval)
	}
}

// The docker client reports a refused connection with an error of its own that
// wraps a message, not the net.Error. It must still count as a transport
// failure, or a daemon that is down without hanging would never be noticed:
// not ejected, not retried at reconcile, its launches answered 500. A
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
