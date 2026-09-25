package cork

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
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

// A worker that has never been admitted is already unresponsive, so the
// verdict its own reconcile reaches is a transition to the state it is in.
// That must still record why — a box that never came up is exactly the one an
// operator has no other way to ask about — while leaving everything that
// describes a *change* alone.
func TestReasonIsRecordedWithoutATransition(t *testing.T) {
	m := &Manager{log: newLogger(DISABLED)}
	w := testWorkerConn(workerUnresponsive) // as newWorkerConn leaves it
	w.since.Store(time.Now().Add(-time.Hour).UnixNano())
	entered := w.since.Load()

	if m.setReachable(w, workerUnresponsive, "its daemon did not answer while it was being reconciled") {
		t.Fatal("storing a reason on the state the worker is already in was reported as a transition")
	}
	r := w.reason.Load()
	if r == nil || *r == "" {
		t.Fatal("a worker that never came up carries no reason: an operator has nothing to go on")
	}
	if w.since.Load() != entered {
		t.Fatal("since moved although the worker did not change state")
	}
	if got := w.ejections.Load(); got != 0 {
		t.Fatalf("ejections %d: a worker that was never admitted was not ejected from anything", got)
	}

	// A later cause replaces an earlier one rather than going unrecorded.
	m.setReachable(w, workerUnresponsive, "dockerd did not answer: connection refused")
	if r := w.reason.Load(); r == nil || *r != "dockerd did not answer: connection refused" {
		t.Fatalf("reason was not refreshed by a later cause: %v", r)
	}
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

// answeringDaemon is a docker daemon that answers /_ping at once and hands
// every other request to other, or a 404 when that is nil: alive, and as slow
// as other makes it, like one deleting an evicted image.
func answeringDaemon(t *testing.T, other http.HandlerFunc) *client.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/_ping") {
			w.Header().Set("Api-Version", "1.44")
			_, _ = w.Write([]byte("OK"))
			return
		}
		if other != nil {
			other(w, r)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	cli, err := client.NewClientWithOpts(client.WithHost("tcp://"+srv.Listener.Addr().String()), client.WithVersion("1.44"))
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	t.Cleanup(func() { cli.Close() })
	return cli
}

// One image removal blocks every create, remove and image inspect on the
// daemon, so on a busy worker it times out many calls together. That is one
// stall against a daemon still answering pings: slow, not dead, and the worker
// stays in placement however many calls it took with it. The probe is still
// woken.
func TestOneStallKeepsTheWorkerHoweverManyCallsItTimesOut(t *testing.T) {
	m := &Manager{log: newLogger(DISABLED), ctx: context.Background()}
	w := testWorkerConn(workerReachableOk)
	w.reconciled.Store(true)
	w.cli = answeringDaemon(t, nil)
	m.workers = map[string]*workerConn{w.ip: w}
	timedOut := fmt.Errorf("inspecting image: %w", context.DeadlineExceeded)

	for i := 1; i <= 3*defaultWorkerTiming.timeoutsToEject; i++ {
		m.noteWorkerTransportError(w.ip, w.cli, timedOut)
		if got := reachableOf(w); got != workerReachableOk {
			t.Fatalf("call %d timed out by one stall ejected the worker: %s", i, got)
		}
		select {
		case <-w.probeNow:
		default:
			t.Fatalf("timeout %d did not wake the probe", i)
		}
	}
}

// Separate stalls do add up, and enough of them within the window eject: a
// daemon wedged underneath a /_ping that answers.
func TestSeparateStallsEjectTheWorker(t *testing.T) {
	timing := defaultWorkerTiming
	timing.controlTimeout = time.Nanosecond // every timeout is a stall of its own
	m := &Manager{log: newLogger(DISABLED), ctx: context.Background(), workerTiming: timing}
	w := testWorkerConn(workerReachableOk)
	w.reconciled.Store(true)
	w.cli = answeringDaemon(t, nil)
	m.workers = map[string]*workerConn{w.ip: w}
	timedOut := fmt.Errorf("creating container: %w", context.DeadlineExceeded)

	limit := timing.timeoutsToEject
	for i := 1; i < limit; i++ {
		m.noteWorkerTransportError(w.ip, w.cli, timedOut)
		if got := reachableOf(w); got != workerReachableOk {
			t.Fatalf("stall %d of %d ejected the worker: %s", i, limit, got)
		}
	}
	m.noteWorkerTransportError(w.ip, w.cli, timedOut)
	if got := reachableOf(w); got != workerUnresponsive {
		t.Fatalf("reachable %s after %d stalls within the window, want unresponsive", got, limit)
	}
}

// A timeout against a daemon that does not answer a ping either is the hung
// daemon the timeout was always meant to catch, and ejects at once -- which is
// what lets a stop against it clear its records instead of failing.
func TestATimeoutAgainstASilentDaemonEjectsAtOnce(t *testing.T) {
	m := &Manager{log: newLogger(DISABLED), ctx: context.Background()}
	w := testWorkerConn(workerReachableOk) // its client dials a port nothing listens on
	w.reconciled.Store(true)
	m.workers = map[string]*workerConn{w.ip: w}

	m.noteWorkerTransportError(w.ip, w.cli, fmt.Errorf("creating container: %w", context.DeadlineExceeded))
	if got := reachableOf(w); got != workerUnresponsive {
		t.Fatalf("reachable %s after a timeout against a daemon that does not answer pings, want unresponsive", got)
	}
}

// A timeout within one call timeout of the last one counted belongs to the
// same stall; only stalls within the window count; an ejection starts the
// count again.
func TestTimeoutsCountStallsWithinTheWindow(t *testing.T) {
	w := testWorkerConn(workerReachableOk)
	start := time.Unix(1_000_000, 0)
	callTimeout, window := 30*time.Second, 2*time.Minute
	for _, step := range []struct {
		after time.Duration
		n     int
		eject bool
	}{
		{0, 1, false},
		{10 * time.Second, 1, false}, // in flight when the first fired: the same stall
		{29 * time.Second, 1, false},
		{40 * time.Second, 2, false},
		{2*time.Minute + time.Second, 2, false}, // the first has aged out
		{2*time.Minute + 31*time.Second, 3, true},
		{2*time.Minute + 32*time.Second, 1, false}, // counted afresh after an ejection
	} {
		n, eject := w.noteTimeout(start.Add(step.after), callTimeout, window, 3)
		if n != step.n || eject != step.eject {
			t.Fatalf("at +%s: got (%d, %t), want (%d, %t)", step.after, n, eject, step.n, step.eject)
		}
	}
}

// A stop whose removals time out against a daemon that answers pings keeps
// the worker and succeeds: the platform does not retry a failed stop, dockerd
// finishes a forced removal it was sent, and docker-reaper takes what is left.
// Every container is still attempted, since each one left running holds its
// port, and so is the network, which does not wait on the layer store.
func TestStopInstanceSucceedsAgainstASlowDaemon(t *testing.T) {
	var containerRemovals, networkRemovals atomic.Int32
	m := setupTestManager(t)
	t.Cleanup(func() { m.db.Close() })
	m.ctx = context.Background()
	timing := defaultWorkerTiming
	timing.controlTimeout = 100 * time.Millisecond
	m.workerTiming = timing

	w := testWorkerConn(workerReachableOk)
	w.reconciled.Store(true)
	w.queue = newDaemonQueue(2)
	w.cli = answeringDaemon(t, func(rw http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/containers/"):
			containerRemovals.Add(1)
			time.Sleep(300 * time.Millisecond) // behind an image removal
			rw.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/networks/"):
			networkRemovals.Add(1)
			rw.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(rw, r)
		}
	})
	m.workers = map[string]*workerConn{w.ip: w}

	challenge := testChallenge("test/slow-stop", 0)
	if errs := m.addChallenges([]*ChallengeMetadata{challenge}); len(errs) > 0 {
		t.Fatalf("addChallenges: %v", errs)
	}
	build := insertTestBuild(t, m, "event", string(challenge.Id), "flag{%s}", 1, 0x1111)
	res, err := m.db.Exec("INSERT INTO instances(build, is_finalized, worker) VALUES (?, 1, ?);", build, w.ip)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()

	instance := &InstanceMetadata{Id: InstanceId(id), Build: build, Worker: w.ip, Containers: []string{"first", "second"}}
	if err := m.stopInstance(instance); err != nil {
		t.Fatalf("a stop against a slow daemon failed: %s", err)
	}
	var rows int
	if err := m.db.Get(&rows, "SELECT COUNT(1) FROM instances WHERE id = ?;", id); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatal("the instance's records were not cleared")
	}
	if got := containerRemovals.Load(); got != 2 {
		t.Fatalf("%d of 2 container removals were attempted", got)
	}
	if got := networkRemovals.Load(); got != 1 {
		t.Fatalf("%d network removals were attempted, want 1", got)
	}
	if got := reachableOf(w); got != workerReachableOk {
		t.Fatalf("a slow stop ejected the worker: %s", got)
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
	t.Setenv(WORKER_TIMEOUTS_TO_EJECT_ENV, "4")
	t.Setenv(WORKER_TIMEOUT_WINDOW_ENV, "90s")
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
		timeoutsToEject:   4,
		timeoutWindow:     90 * time.Second,
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
