package cmgr

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

func reconcileTestConn() *workerConn {
	w := &workerConn{ip: "10.0.0.1", done: make(chan struct{})}
	w.health.Store(int32(workerOverloaded))
	return w
}

// A daemon that is still starting is retried, the worker staying out of
// placement as overloaded rather than going down.
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
	if !m.reconcileWithRetries(w, reconcile, time.Second, time.Millisecond) {
		t.Fatal("a daemon that came up within the budget was not waited for")
	}
	if attempts != 3 {
		t.Fatalf("reconcile attempted %d times, want 3", attempts)
	}
	if got := healthOf(w); got != workerOverloaded {
		t.Fatalf("health while waiting: %s, want overloaded", got)
	}
}

// A daemon still unreachable once the budget is spent marks the worker down.
func TestReconcileWithRetriesGivesUp(t *testing.T) {
	m := reconcileTestManager()
	w := reconcileTestConn()
	never := func(*workerConn) reconcileResult { return reconcileUnreachable }
	if m.reconcileWithRetries(w, never, 20*time.Millisecond, time.Millisecond) {
		t.Fatal("an unreachable daemon was reported ready to poll")
	}
	if got := healthOf(w); got != workerDown {
		t.Fatalf("health after the budget: %s, want down", got)
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
	if !m.reconcileWithRetries(w, incomplete, 20*time.Millisecond, time.Millisecond) {
		t.Fatal("an incomplete reconcile kept the worker out of placement")
	}
	if attempts < 2 {
		t.Fatalf("an incomplete reconcile was attempted %d time(s), want a retry", attempts)
	}
	if got := healthOf(w); got != workerOverloaded {
		t.Fatalf("an incomplete reconcile changed the health: %s", got)
	}
}

// A conn replaced or removed while waiting is left alone: not polled, not
// marked down.
func TestReconcileWithRetriesStopsWhenGone(t *testing.T) {
	m := reconcileTestManager()
	w := reconcileTestConn()
	never := func(*workerConn) reconcileResult { return reconcileUnreachable }
	close(w.done)
	if m.reconcileWithRetries(w, never, time.Hour, time.Hour) {
		t.Fatal("a gone conn was reported ready to poll")
	}
	if got := healthOf(w); got != workerOverloaded {
		t.Fatalf("a gone conn had its health changed: %s", got)
	}
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
	if got := healthOf(w); got != workerOverloaded {
		t.Fatalf("a removal failure changed the health itself: %s", got)
	}

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
