package cork

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
)

func TestNetworkInstance(t *testing.T) {
	cases := map[string]struct {
		id InstanceId
		ok bool
	}{
		"cmgr-7":     {7, true},
		"cmgr-12345": {12345, true},
		"cmgr-":      {0, false},
		"cmgr-x":     {0, false},
		"cmgr-7a":    {0, false},
		"cmgr--7":    {0, false},
		"cmgr-+7":    {0, false},
		"cmgr-net":   {0, false},
		"xcmgr-7":    {0, false},
		"bridge":     {0, false},
	}
	for name, want := range cases {
		id, ok := networkInstance(name)
		if ok != want.ok || id != want.id {
			t.Errorf("networkInstance(%q) = %d, %v; want %d, %v", name, id, ok, want.id, want.ok)
		}
	}
	// The parser must agree with the namer.
	inst := &InstanceMetadata{Id: 42}
	if id, ok := networkInstance(inst.getNetworkName()); !ok || id != 42 {
		t.Fatalf("networkInstance(getNetworkName()) = %d, %v", id, ok)
	}
}

func summaryOn(id string, networks ...string) container.Summary {
	c := container.Summary{ID: id}
	if len(networks) > 0 {
		c.NetworkSettings = &container.NetworkSettingsSummary{Networks: map[string]*network.EndpointSettings{}}
		for _, n := range networks {
			c.NetworkSettings.Networks[n] = &network.EndpointSettings{}
		}
	}
	return c
}

func netSummary(name string) network.Summary {
	return network.Summary{Network: network.Network{Name: name}}
}

func TestFindOrphans(t *testing.T) {
	live := map[InstanceId]bool{3: true}
	containers := []container.Summary{
		summaryOn("live", "cmgr-3"),      // its row exists on this worker
		summaryOn("orphan", "cmgr-4"),    // no row: a DB-only stop left it
		summaryOn("elsewhere", "cmgr-5"), // id 5 may live on another worker; here it is an orphan
		summaryOn("unattributed"),        // no cmgr network: left alone
		summaryOn("foreign", "bridge"),   // not on a cmgr network: left alone
	}
	networks := []network.Summary{
		netSummary("cmgr-3"), netSummary("cmgr-4"), netSummary("bridge"), netSummary("cmgr-net"),
	}

	cids, nets := findOrphans(containers, networks, live)
	if want := []string{"orphan", "elsewhere"}; !reflect.DeepEqual(cids, want) {
		t.Errorf("orphaned containers = %v, want %v", cids, want)
	}
	if want := []string{"cmgr-4"}; !reflect.DeepEqual(nets, want) {
		t.Errorf("orphaned networks = %v, want %v", nets, want)
	}
}

func TestFindOrphansNothingToDo(t *testing.T) {
	cids, nets := findOrphans(nil, nil, nil)
	if len(cids) != 0 || len(nets) != 0 {
		t.Fatalf("got %v and %v from nothing", cids, nets)
	}
}

func reconcileTestManager() *Manager {
	return &Manager{log: newLogger(DISABLED), ctx: context.Background()}
}

// notEjected asserts a pass did not eject the worker. It has to read
// daemonFailed, not reachability or the ejection count: every conn starts
// unresponsive, so an eject that should not have happened moves neither of
// those and an assertion on them holds under exactly the bug it guards.
// daemonFailed is set on every ejection regardless, and is what the stop path
// reads, so it is the honest witness.
func notEjected(t *testing.T, w *workerConn, what string) {
	t.Helper()
	if w.daemonFailed.Load() {
		t.Fatalf("%s ejected the worker: removeOrphans reports, reconcileWithRetries decides", what)
	}
	if got := w.ejections.Load(); got != 0 {
		t.Fatalf("%s counted %d ejection(s)", what, got)
	}
}

func reconcileTestConn() *workerConn {
	w := &workerConn{ip: "10.0.0.1", done: make(chan struct{}), unreachCh: make(chan struct{})}
	// As newWorkerConn leaves it: nothing has been heard from the box yet.
	w.reachable.Store(int32(workerUnresponsive))
	w.load.Store(int32(workerLoadUnknown))
	return w
}

// A daemon that is still starting is retried, the worker staying out of
// placement meanwhile and joining it once the pass finishes.
func TestReconcileWithRetriesWaitsForTheDaemon(t *testing.T) {
	m := reconcileTestManager()
	w := reconcileTestConn()
	attempts := 0
	reconcile := func(*workerConn) reconcileResult {
		attempts++
		if attempts >= 3 {
			return reconcileDone
		}
		return reconcileUnreachable
	}
	if got := m.reconcileWithRetries(w, reconcile, time.Second, time.Millisecond); got != reconcileReady {
		t.Fatalf("a daemon that came up within the budget was not waited for: %d", got)
	}
	if attempts != 3 {
		t.Fatalf("reconcile attempted %d times, want 3", attempts)
	}
	// The pass does not admit the worker itself; runWorker does, once it has
	// the outcome.
	if got := reachableOf(w); got != workerUnresponsive {
		t.Fatalf("reachable while waiting: %s, want unresponsive", got)
	}
}

// A daemon still unreachable once the budget is spent ejects the worker --
// reversibly, so that the probe brings it back rather than an operator.
func TestReconcileWithRetriesGivesUp(t *testing.T) {
	m := reconcileTestManager()
	w := reconcileTestConn()
	never := func(*workerConn) reconcileResult { return reconcileUnreachable }
	if got := m.reconcileWithRetries(w, never, 20*time.Millisecond, time.Millisecond); got != reconcileEjected {
		t.Fatalf("an unreachable daemon: got outcome %d, want ejected", got)
	}
	if got := reachableOf(w); got != workerUnresponsive {
		t.Fatalf("reachable after the budget: %s, want unresponsive", got)
	}
	// Unresponsive, not down: the poller keeps probing and nothing here needs
	// an operator to undo it. No ejection is counted, because the worker was
	// never admitted in the first place -- a box that is slow to boot should
	// not start its recovery backoff already stretched.
	if got := w.ejections.Load(); got != 0 {
		t.Fatalf("a worker that never joined placement counted %d ejection(s)", got)
	}
}

// A pass that reaches the daemon but cannot finish (a database read failed, a
// removal was refused) is retried too, but the worker joins placement once
// the budget is spent rather than being taken out of the fleet.
func TestReconcileWithRetriesJoinsWhenIncomplete(t *testing.T) {
	m := reconcileTestManager()
	w := reconcileTestConn()
	attempts := 0
	incomplete := func(*workerConn) reconcileResult {
		attempts++
		return reconcileIncomplete
	}
	if got := m.reconcileWithRetries(w, incomplete, 20*time.Millisecond, time.Millisecond); got != reconcileReady {
		t.Fatalf("an incomplete reconcile kept the worker out of placement: outcome %d", got)
	}
	if attempts < 2 {
		t.Fatalf("an incomplete reconcile was attempted %d time(s), want a retry", attempts)
	}
	notEjected(t, w, "an incomplete reconcile")
}

// A conn replaced or removed while waiting is left alone: not polled, not
// ejected.
func TestReconcileWithRetriesStopsWhenGone(t *testing.T) {
	m := reconcileTestManager()
	w := reconcileTestConn()
	never := func(*workerConn) reconcileResult { return reconcileUnreachable }
	close(w.done)
	if got := m.reconcileWithRetries(w, never, time.Hour, time.Hour); got != reconcileAbandoned {
		t.Fatalf("a gone conn: got outcome %d, want abandoned", got)
	}
	notEjected(t, w, "a gone conn")
}

// Removals run a few at a time, every one is counted, and a transport failure
// reports the daemon unreachable and stops the pass.
func TestRemoveOrphansBoundedParallel(t *testing.T) {
	m := reconcileTestManager()
	w := reconcileTestConn()
	names := make([]string, 20)
	for i := range names {
		names[i] = "c" + string(rune('a'+i))
	}
	var inFlight, peak atomic.Int32
	removed, result := m.removeOrphans(w, "container", names, func(ctx context.Context, name string) error {
		cur := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			p := peak.Load()
			if cur <= p || peak.CompareAndSwap(p, cur) {
				break
			}
		}
		time.Sleep(2 * time.Millisecond)
		return nil
	})
	if result != reconcileDone || removed != len(names) {
		t.Fatalf("removed %d of %d, result %s", removed, len(names), result)
	}
	if p := peak.Load(); p > reconcileParallelism || p < 2 {
		t.Fatalf("peak in-flight removals %d, want between 2 and %d", p, reconcileParallelism)
	}

	var calls atomic.Int32
	removed, result = m.removeOrphans(w, "container", names, func(ctx context.Context, name string) error {
		calls.Add(1)
		return context.DeadlineExceeded
	})
	if result != reconcileUnreachable || removed != 0 {
		t.Fatalf("a hung removal reported %s or counted %d", result, removed)
	}
	if c := calls.Load(); c > reconcileParallelism*2 {
		t.Fatalf("%d removals were attempted after the daemon hung, want the pass to stop", c)
	}
	// removeOrphans reports; it does not decide. Ejecting is reconcileWithRetries'
	// call, once the whole budget is spent.
	if got := reachableOf(w); got != workerUnresponsive {
		t.Fatalf("a removal failure changed reachability itself: %s", got)
	}
	notEjected(t, w, "a removal failure")

	// A removal the daemon refuses leaves the pass incomplete, but every
	// other orphan is still attempted: the ones that go, go.
	calls.Store(0)
	removed, result = m.removeOrphans(w, "container", names, func(ctx context.Context, name string) error {
		if calls.Add(1) == 1 {
			return errors.New("removal already in progress")
		}
		return nil
	})
	if result != reconcileIncomplete {
		t.Fatalf("a refused removal reported %s, want incomplete", result)
	}
	if removed != len(names)-1 {
		t.Fatalf("removed %d of %d after one refusal", removed, len(names)-1)
	}
}
