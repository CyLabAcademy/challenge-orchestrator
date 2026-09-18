package cork

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/moby/moby/client"
)

// fakeDaemon is enough of a dockerd for the two probes: the /_ping the poller
// runs every tick, and the container list it runs on a slower one. Each can be
// failed independently, which is the whole point — a daemon that answers pings
// while its container store is wedged is the failure the deep probe was added
// to catch, and it cannot be reproduced against a real daemon in a unit test.
type fakeDaemon struct {
	srv       *httptest.Server
	listFails atomic.Bool
	listHangs atomic.Bool // accepts the list and never answers, until the caller gives up
	pingFails atomic.Bool
	lists     atomic.Int32
}

func newFakeDaemon(t *testing.T) *fakeDaemon {
	t.Helper()
	d := &fakeDaemon{}
	d.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/_ping"):
			if d.pingFails.Load() {
				http.Error(w, "no", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Api-Version", "1.44")
			w.Header().Set("Ostype", "linux")
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			d.lists.Add(1)
			if d.listHangs.Load() {
				// The wedge that matters: the daemon takes the request and
				// never answers, so the caller leaves on its own timeout
				// rather than on an error. A store that returns 500 promptly
				// is the easy case and exercises none of the timing.
				<-r.Context().Done()
				return
			}
			if d.listFails.Load() {
				http.Error(w, "container store is wedged", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, "[]")
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(d.srv.Close)
	return d
}

// conn builds a worker pointed at the fake daemon, admitted and registered as
// the poller expects to find it. The IP is loopback so that the telemetry poll
// this worker has no agent for is refused at once rather than waiting out its
// timeout on every tick.
func (d *fakeDaemon) conn(t *testing.T, m *Manager) *workerConn {
	t.Helper()
	cli, err := client.NewClientWithOpts(
		client.WithHost(strings.Replace(d.srv.URL, "http://", "tcp://", 1)),
		client.WithVersion("1.44"),
	)
	if err != nil {
		t.Fatalf("building a client for the fake daemon: %s", err)
	}
	w := &workerConn{
		ip:        "127.0.0.1",
		cli:       cli,
		queue:     newDaemonQueue(1),
		unreachCh: make(chan struct{}),
		done:      make(chan struct{}),
		probeNow:  make(chan struct{}, 1),
	}
	w.reachable.Store(int32(workerReachableOk))
	w.load.Store(int32(workerLoadOk))
	w.since.Store(time.Now().UnixNano())
	w.reconciled.Store(true)
	m.workers = map[string]*workerConn{w.ip: w}
	t.Cleanup(func() { close(w.done) })
	return w
}

// probeTiming is the production shape with the clock wound down: the deep
// probe stays several ticks apart from the ping, which is the ratio the
// counter has to survive.
func probeTiming() workerTiming {
	return workerTiming{
		pollInterval:      50 * time.Millisecond,
		pollTimeout:       10 * time.Millisecond,
		pingTimeout:       20 * time.Millisecond,
		deepProbeInterval: 150 * time.Millisecond,
		maxMisses:         6,
		loadMisses:        1000, // not what these tests are about
		healthyThreshold:  2,
		recoverBackoff:    time.Millisecond,
		recoverBackoffMax: time.Millisecond,
		ejectionDecay:     time.Hour,
		// Deliberately far larger than the interval, exactly as in production
		// (30s against 5s). The deep probe must not inherit it: if it does, a
		// hung container store parks the poll goroutine for a ceiling at a
		// time and the ejection window stretches by that ratio. Leaving this
		// big is what gives TestAHungContainerStoreEjectsOnTheProbeCadence
		// something to fail against.
		controlTimeout: 2 * time.Second,
	}
}

func eventually(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", within, what)
}

// A daemon that answers /_ping while its container store is wedged must still
// be ejected. This is the failure the deep probe exists for, and the one the
// miss counter is most easily made blind to: the deep probe runs a few ticks
// apart, so if an intervening ping could clear the run of misses the threshold
// would never be reached at all, however long the store stayed wedged.
func TestAWedgedContainerStoreEjectsTheWorker(t *testing.T) {
	d := newFakeDaemon(t)
	d.listFails.Store(true)

	m := &Manager{log: newLogger(DISABLED), ctx: context.Background(), workerTiming: probeTiming()}
	w := d.conn(t, m)
	go m.pollWorker(w)

	eventually(t, 5*time.Second, "a worker whose container store is wedged to be ejected", func() bool {
		return reachableOf(w) == workerUnresponsive
	})
	if got := d.lists.Load(); got < 1 {
		t.Fatal("the worker was ejected without the deep probe ever running")
	}
	r := w.reason.Load()
	if r == nil || !strings.Contains(*r, "could not list containers") {
		t.Fatalf("reason %v does not name the probe that failed, so an operator cannot tell this from a dead daemon", r)
	}
}

// The mirror of the above: a daemon that is fully healthy keeps its place
// however many deep probes run, so the latch above cannot eject on its own.
func TestAHealthyDaemonIsNotEjectedByTheDeepProbe(t *testing.T) {
	d := newFakeDaemon(t)

	m := &Manager{log: newLogger(DISABLED), ctx: context.Background(), workerTiming: probeTiming()}
	w := d.conn(t, m)
	go m.pollWorker(w)

	eventually(t, 5*time.Second, "several deep probes to run", func() bool { return d.lists.Load() >= 3 })
	if got := reachableOf(w); got != workerReachableOk {
		t.Fatalf("a healthy daemon was ejected: %s (%v)", got, w.reason.Load())
	}
}

// A container store that fails once and then works clears the latch, so the
// worker is not ejected for a blip.
//
// The latch is the whole reason a ping-only tick can count as a miss, so a
// latch that never cleared would turn one bad list into an ejection a few
// ticks later however healthy the daemon became. This test therefore asserts
// the worker KEEPS its place, over a window several deep-probe intervals long
// — long enough that a stuck latch would have crossed maxMisses well inside
// it. Asserting on readyToRecover instead, as an earlier version did, proved
// nothing: with a short backoff and an old `since` it is true whatever the
// latch does.
func TestAContainerStoreThatRecoversClearsTheLatch(t *testing.T) {
	d := newFakeDaemon(t)
	d.listFails.Store(true)

	timing := probeTiming()
	timing.maxMisses = 8
	m := &Manager{log: newLogger(DISABLED), ctx: context.Background(), workerTiming: timing}
	w := d.conn(t, m)
	go m.pollWorker(w)

	// One failed deep probe, which latches.
	eventually(t, 5*time.Second, "the first deep probe to fail", func() bool { return d.lists.Load() >= 1 })
	d.listFails.Store(false)

	// From here the daemon is entirely healthy. Watch for long enough that a
	// latch still set would have ejected the worker several times over:
	// maxMisses ticks is 400ms, and this watches for twice that.
	deadline := time.Now().Add(800 * time.Millisecond)
	for time.Now().Before(deadline) {
		if got := reachableOf(w); got != workerReachableOk {
			t.Fatalf("worker went %s after a single failed container list against a daemon that has been healthy since: the latch never cleared, so every ping counts as a miss forever (%v)", got, w.reason.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if d.lists.Load() < 2 {
		t.Fatal("no deep probe ran after the store recovered, so nothing could have cleared the latch and this proved nothing")
	}
}

// A container store that ACCEPTS the list and never answers must be ejected on
// the probe cadence, not on the control timeout.
//
// This is the failure the deep probe exists for and the one it is easiest to
// get wrong. The probe has to carry its own ceiling: controlTimeout is sized
// for a create or a remove and is many times the poll interval, so a probe
// inheriting it parks the poll goroutine for a ceiling at a time. Worse, when
// that ceiling is as long as the probe's own interval, each hung call leaves
// the next tick already due for another — the probe becomes the tick, misses
// advance once per ceiling instead of once per interval, and the ejection
// window silently stretches by that ratio.
//
// A store returning 500 promptly does not exercise any of this, which is why
// the test above it passed while this was broken.
func TestAHungContainerStoreEjectsOnTheProbeCadence(t *testing.T) {
	d := newFakeDaemon(t)
	d.listHangs.Store(true)

	timing := probeTiming()
	m := &Manager{log: newLogger(DISABLED), ctx: context.Background(), workerTiming: timing}
	w := d.conn(t, m)
	go m.pollWorker(w)

	// maxMisses ticks is 300ms. A budget of 1.5s leaves generous room for a
	// loaded machine while staying far below the 12s (maxMisses x
	// controlTimeout) that inheriting the control timeout would cost.
	start := time.Now()
	eventually(t, 1500*time.Millisecond, "a worker whose container store hangs to be ejected on the probe cadence", func() bool {
		return reachableOf(w) == workerUnresponsive
	})
	if took := time.Since(start); took > time.Duration(timing.maxMisses)*timing.controlTimeout/2 {
		t.Fatalf("ejection took %s: the deep probe is running on the control timeout rather than its own", took)
	}
	r := w.reason.Load()
	if r == nil || !strings.Contains(*r, "could not list containers") {
		t.Fatalf("reason %v does not name the probe that failed", r)
	}
}

// An operator taking a box down while a probe runs must not have that
// assertion dropped. The window is not a tight race: a deep probe can sit in
// a container list for the whole control timeout, and worker-down before a
// reboot is exactly what an operator does to a worker that has been flapping.
//
// setReachable refuses to lift an asserted down, but recovery does not go
// through setReachable -- it discards the conn holding the assertion for a
// fresh one that carries nothing.
func TestRecoveryDoesNotResurrectAnAssertedDownWorker(t *testing.T) {
	m := &Manager{log: newLogger(DISABLED), ctx: context.Background()}
	w := testWorkerConn(workerUnresponsive)
	w.done = make(chan struct{})
	m.workers = map[string]*workerConn{w.ip: w}

	if err := m.SetWorkerDown(w.ip); err != nil {
		t.Fatalf("worker-down: %s", err)
	}
	m.recoverWorker(w)

	if got := reachableOf(w); got != workerDown {
		t.Fatalf("reachable %s after a recovery raced an operator's worker-down, want down", got)
	}
	if m.workers[w.ip] != w {
		t.Fatal("recovery replaced the conn carrying an operator's assertion, so the worker rejoins placement while the box is being rebooted")
	}
	select {
	case <-w.done:
		t.Fatal("recovery stopped the poller of a worker an operator had taken down")
	default:
	}
}

// The same invariant under the interleaving that produces it: worker-down and
// a recovery landing together. Whichever wins, the worker an operator asked
// for must end up down -- never quietly back in placement.
//
// What this pins is that BOTH COMPLETED ORDERINGS end with the worker down:
// recovery swapping before worker-down, and worker-down landing before a
// recovery that then declines. The log line records the split, because a run
// that only ever produced one of them would be pinning half of what it claims.
//
// What it does NOT pin, and cannot, is SetWorkerDown's lock discipline. The
// interleaving that discipline exists to exclude needs recoverWorker to take
// the write lock inside the window between SetWorkerDown's map read and its
// assertion. Hold the read lock across both, as the code does, and the window
// does not exist; release it, as the code used to, and the window is a map
// read wide -- a few nanoseconds that no amount of looping here will reliably
// land in. Reverting that release does not fail this test. Testing it properly
// would mean a hook in SetWorkerDown, and a test seam in the control plane is
// a worse trade than an untested nanosecond. It is held by the code reading
// as it does and by TestWorkerDownLandsOnTheConnThatRecoveryInstalled below,
// which covers the outcome rather than the mechanism.
//
// Both orderings DO complete, which an earlier version of this test denied: it
// claimed newWorkerConn cannot succeed without TLS material. It can --
// workerTLSConfig returns a nil config when DOCKER_CERT_PATH is unset and a
// plaintext client is built instead -- so every round recoverWorker won ran
// the real swap and then crashed on the replaced conn's nil client, taking the
// package's whole test binary with it. That is also why this test's assertion
// was never once reached on that path.
func TestWorkerDownAndRecoveryCannotBothWin(t *testing.T) {
	// Small enough that a conn replaced here gives up on its unreachable
	// address quickly: the swap starts a real poller against 10.0.0.1:2376.
	timing := probeTiming()
	timing.controlTimeout = 5 * time.Millisecond
	timing.pollInterval = 5 * time.Millisecond
	timing.maxMisses = 1

	var swapped, declined int
	for i := 0; i < 100; i++ {
		m := &Manager{log: newLogger(DISABLED), ctx: context.Background(), workerTiming: timing}
		w := testWorkerConn(workerUnresponsive)
		w.done = make(chan struct{})
		m.workers = map[string]*workerConn{w.ip: w}

		done := make(chan struct{})
		go func() {
			defer close(done)
			m.recoverWorker(w)
		}()
		// Give the recovery a head start on half the rounds. Left to itself
		// the main goroutine wins every time -- the spawned one has not been
		// scheduled yet -- and the swap ordering, which is the one a lost lock
		// would corrupt, never runs at all.
		if i%2 == 1 {
			time.Sleep(200 * time.Microsecond)
		}
		if err := m.SetWorkerDown(w.ip); err != nil {
			t.Fatalf("worker-down: %s", err)
		}
		<-done

		m.workersMu.RLock()
		cur := m.workers[w.ip]
		m.workersMu.RUnlock()
		if got := reachableOf(cur); got != workerDown {
			t.Fatalf("round %d: the current conn is %s, so an operator's worker-down was written to a conn that had already been replaced", i, got)
		}
		if cur != w {
			swapped++
			close(cur.done) // stop the poller the swap started
		} else {
			declined++
		}
	}
	// Not an assertion about the scheduler — one-sided, so it cannot flake —
	// but a record of what was actually exercised. If this only ever reports
	// declines, the test is pinning one ordering and the other is untested.
	t.Logf("recoverWorker completed the swap in %d of %d rounds, declined in %d", swapped, swapped+declined, declined)
}

// The swap ordering on its own, without a scheduler in the way: a recovery
// that has already completed, and then the operator's worker-down.
//
// This pins the outcome the lock discipline protects, rather than the
// discipline itself: worker-down must reach the conn the fleet is currently
// using, never one that recovery has already discarded. It fails against the
// whole family of bugs where the target conn is captured too early -- a
// caller that holds a *workerConn across a recovery, a helper that looks the
// worker up once and reuses it -- which is the shape the real defect took.
// Get it wrong and worker-list reports down while the conn actually serving
// reconciles and takes placements on a box the operator is rebooting.
func TestWorkerDownLandsOnTheConnThatRecoveryInstalled(t *testing.T) {
	timing := probeTiming()
	timing.controlTimeout = 5 * time.Millisecond
	timing.pollInterval = 5 * time.Millisecond
	timing.maxMisses = 1

	m := &Manager{log: newLogger(DISABLED), ctx: context.Background(), workerTiming: timing}
	w := testWorkerConn(workerUnresponsive)
	w.done = make(chan struct{})
	m.workers = map[string]*workerConn{w.ip: w}

	m.recoverWorker(w)
	fresh := m.workers[w.ip]
	if fresh == w {
		t.Fatal("recovery did not replace the conn, so this test is not exercising the ordering it exists for")
	}
	t.Cleanup(func() { close(fresh.done) })

	if err := m.SetWorkerDown(w.ip); err != nil {
		t.Fatalf("worker-down: %s", err)
	}
	if got := reachableOf(fresh); got != workerDown {
		t.Fatalf("the conn the fleet is using is %s: worker-down was written to the conn recovery had already discarded", got)
	}
	// And the recovery this replaced must have carried its ejection count
	// forward, or a box that keeps failing is retried at the base wait every
	// time however often it has failed.
	if fresh.fromRecovery != true {
		t.Fatal("a conn built by recoverWorker is not marked as such, so an ejection ending its recovery attempt will not be counted")
	}
}

// A conn that has not yet proved itself is not the same as one that has been
// given up on, and only the second means a docker call is pointless. Every
// conn starts unresponsive -- at corkd startup, on worker-add against a live
// box, and on recovery -- and spends its reconcile pass there with a daemon
// that is usually answering perfectly well. Treating that as unreachable has
// the stop path delete an instance's records with no teardown, leaving the
// container serving under its restart policy while its host port goes back
// into the pool.
func TestAConnThatHasNotProvedItselfIsStillWorthCalling(t *testing.T) {
	m := &Manager{log: newLogger(DISABLED)}
	w := testWorkerConn(workerUnresponsive) // as newWorkerConn leaves it
	m.workers = map[string]*workerConn{w.ip: w}

	if m.workerUnreachable(w.ip) {
		t.Fatal("a worker whose first reconcile is still running was called unreachable: its stop path skips docker on a healthy box")
	}

	// Once its daemon has actually failed something, it is unreachable --
	// including when the verdict came from its own reconcile pass, which
	// changes no state because the conn started unresponsive.
	m.eject(w, "its daemon did not answer while the worker was being reconciled")
	if !m.workerUnreachable(w.ip) {
		t.Fatal("a worker its own reconcile gave up on was still called reachable")
	}

	// And down is unreachable however it was reached.
	down := testWorkerConn(workerReachableOk)
	m.workers = map[string]*workerConn{down.ip: down}
	m.markWorkerDown(down)
	if !m.workerUnreachable(down.ip) {
		t.Fatal("a worker an operator took down was called reachable")
	}
}

// An ejection by a failed control call is the one the backoff exists to
// throttle -- it is discovered by a launch paying a full control timeout, not
// by a probe -- so it must start the wait like any other.
func TestTheRecoveryBackoffMeasuresFromTheEjection(t *testing.T) {
	timing := probeTiming()
	timing.recoverBackoff = time.Hour
	timing.recoverBackoffMax = time.Hour
	m := &Manager{log: newLogger(DISABLED), workerTiming: timing}

	w := testWorkerConn(workerReachableOk)
	w.since.Store(time.Now().Add(-24 * time.Hour).UnixNano()) // healthy all day
	m.eject(w, "a control call failed: context deadline exceeded")

	if m.readyToRecover(w, 1000, timing) {
		t.Fatal("a worker ejected by a failed control call skipped its recovery backoff: the ejection did not start the clock")
	}

	// And the threshold is a floor of its own, not something a long wait buys.
	w.since.Store(time.Now().Add(-24 * time.Hour).UnixNano())
	if m.readyToRecover(w, timing.healthyThreshold-1, timing) {
		t.Fatal("a worker was recovered on fewer consecutive good probes than the threshold")
	}
	if !m.readyToRecover(w, timing.healthyThreshold, timing) {
		t.Fatal("a worker that waited out its backoff and answered enough probes was not recovered")
	}
}

// An ejection that ends a recovery attempt must be counted, or the backoff
// never grows for the box that most needs it: one whose daemon answers pings
// but whose reconcile keeps failing rebuilds a connection, spawns a poller and
// hammers the daemon for a fresh budget every backoff, forever, at a fixed
// period. setReachable reports no transition there -- a conn recoverWorker
// builds already enters unresponsive -- so the count has to come from
// somewhere else, and fromRecovery is that somewhere.
//
// This also covers the only path on which a recovery-built conn's own
// goroutine reads fromRecovery, so it is what holds that field's one
// synchronisation rule: set before newWorkerConn starts the goroutine, never
// after. Run under -race, writing it later reports one.
func TestAFailedRecoveryCountsAnEjection(t *testing.T) {
	timing := probeTiming()
	timing.controlTimeout = 5 * time.Millisecond
	timing.pollInterval = 5 * time.Millisecond
	timing.pingTimeout = 5 * time.Millisecond
	timing.maxMisses = 1
	m := &Manager{log: newLogger(DISABLED), ctx: context.Background(), workerTiming: timing}

	w := testWorkerConn(workerUnresponsive)
	w.done = make(chan struct{})
	w.ejections.Store(3) // this box has failed before
	m.workers = map[string]*workerConn{w.ip: w}

	m.recoverWorker(w)
	fresh := m.workers[w.ip]
	if fresh == w {
		t.Fatal("recovery did not replace the conn, so nothing here is being tested")
	}
	t.Cleanup(func() { close(fresh.done) })

	// Its reconcile dials an address nothing answers on and gives up.
	eventually(t, 5*time.Second, "the recovery's own reconcile to give up on the daemon", func() bool {
		return fresh.daemonFailed.Load()
	})
	// And then, separately, for the count. eject sets daemonFailed before it
	// decides whether to increment (it has to: the flag is what tells the stop
	// path the daemon has actually failed, and it is set outside the
	// transition), so waiting on the flag and reading the counter in the next
	// breath can catch the gap between the two and accuse this fix of a
	// regression it does not have. The assertion below keeps its message for
	// the regression it was written for.
	eventually(t, time.Second, "the ejection that ended the recovery to be counted", func() bool {
		return fresh.ejections.Load() != 3
	})
	if got := fresh.ejections.Load(); got != 4 {
		t.Fatalf("ejections %d after a failed recovery, want 4: the attempt ended in an ejection that was not counted, so the next recovery waits no longer than this one did", got)
	}
}

// One slow container list must not be fatal.
//
// The latch is cleared only by a deep probe that succeeds, and the ordinary
// deep cadence is no shorter than the whole ejection budget -- so if a
// suspicion could only be re-tested on that cadence, a single list that merely
// took too long would eject the worker with certainty: every tick until the
// threshold counts as a miss and none of them can be disproved. A degraded box
// still ejects, because then every deep probe fails; what must not happen is a
// healthy one going out on the strength of one bad sample.
func TestOneSlowContainerListIsNotFatal(t *testing.T) {
	d := newFakeDaemon(t)
	d.listHangs.Store(true) // the first list will time out

	timing := probeTiming()
	// The production shape, and the one that made this certain: a deep cadence
	// no shorter than maxMisses ticks.
	timing.deepProbeInterval = time.Duration(timing.maxMisses) * timing.pollInterval
	m := &Manager{log: newLogger(DISABLED), ctx: context.Background(), workerTiming: timing}
	w := d.conn(t, m)
	go m.pollWorker(w)

	// Let the first deep probe fail, then let the store answer again.
	eventually(t, 5*time.Second, "the first deep probe to time out", func() bool { return d.lists.Load() >= 1 })
	d.listHangs.Store(false)

	// From here the daemon is healthy. Watch for well over the ejection budget:
	// if the suspicion could not be re-tested until the next slow-cadence deep
	// probe, the worker would be gone before it arrived.
	deadline := time.Now().Add(time.Duration(timing.maxMisses+4) * timing.pollInterval)
	for time.Now().Before(deadline) {
		if got := reachableOf(w); got != workerReachableOk {
			t.Fatalf("worker went %s after a single slow container list against a store that has answered ever since (%v): a suspicion has to be re-testable before it becomes an ejection", got, w.reason.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if d.lists.Load() < 2 {
		t.Fatal("the store was never listed again, so nothing could have cleared the suspicion and this proved nothing")
	}
}
