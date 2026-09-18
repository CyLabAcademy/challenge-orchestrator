package cork

import (
	"context"
	"testing"
	"time"
)

// addWorkerTestManager is a manager AddWorker can actually run against: a real
// database, and timings small enough that the poller AddWorker starts gives up
// on its unreachable address at once instead of outliving the test.
func addWorkerTestManager(t *testing.T) *Manager {
	t.Helper()
	m := setupTestManager(t)
	t.Cleanup(func() { m.db.Close() })
	m.ctx = context.Background()
	m.workers = map[string]*workerConn{}
	m.workerTiming = workerTiming{
		pollInterval:      5 * time.Millisecond,
		pollTimeout:       time.Millisecond,
		pingTimeout:       time.Millisecond,
		deepProbeInterval: time.Hour,
		maxMisses:         1,
		loadMisses:        1000,
		healthyThreshold:  1000, // never recover; nothing here is coming back
		recoverBackoff:    time.Hour,
		recoverBackoffMax: time.Hour,
		ejectionDecay:     time.Hour,
		controlTimeout:    5 * time.Millisecond,
	}
	t.Cleanup(func() {
		m.workersMu.Lock()
		defer m.workersMu.Unlock()
		for _, w := range m.workers {
			close(w.done)
		}
	})
	return m
}

func storedPublic(t *testing.T, m *Manager, ip string) string {
	t.Helper()
	var public string
	if err := m.db.Get(&public, "SELECT public FROM workers WHERE ip = ?;", ip); err != nil {
		t.Fatalf("reading back the public address of %s: %s", ip, err)
	}
	return public
}

// worker-add is how a worker is registered AND how an operator forces an
// immediate reconnect on one that is already registered -- TROUBLESHOOTING
// says so in as many words, and the one-argument form is what they will type.
// That form sends no public address, and the column it would overwrite is the
// address players are given for every instance placed there.
func TestAddWorkerKeepsThePublicAddressWhenItIsOmitted(t *testing.T) {
	const ip = "127.0.0.1"
	m := addWorkerTestManager(t)

	if err := m.AddWorker(ip, "worker-a.example"); err != nil {
		t.Fatalf("first worker-add: %s", err)
	}
	if got := storedPublic(t, m, ip); got != "worker-a.example" {
		t.Fatalf("stored public %q after the first add, want worker-a.example", got)
	}

	// The reconnect an operator runs. It must not move a single player.
	if err := m.AddWorker(ip, ""); err != nil {
		t.Fatalf("worker-add with no public address: %s", err)
	}
	if got := storedPublic(t, m, ip); got != "worker-a.example" {
		t.Fatalf("stored public %q after re-adding with no address, want worker-a.example: every instance placed here afterwards would be handed a different address from the ones already running on it", got)
	}

	// And the conn agrees with the table, or the two disagree about where
	// players are sent depending on which one is asked.
	m.workersMu.RLock()
	w := m.workers[ip]
	m.workersMu.RUnlock()
	if w == nil {
		t.Fatal("no conn registered for the worker")
	}
	if w.public != "worker-a.example" {
		t.Fatalf("conn public %q, want worker-a.example", w.public)
	}
}

// Passing one still sets it: this is how an address is changed.
func TestAddWorkerSetsThePublicAddressWhenGiven(t *testing.T) {
	const ip = "127.0.0.1"
	m := addWorkerTestManager(t)

	if err := m.AddWorker(ip, "old.example"); err != nil {
		t.Fatalf("first worker-add: %s", err)
	}
	if err := m.AddWorker(ip, "new.example"); err != nil {
		t.Fatalf("second worker-add: %s", err)
	}
	if got := storedPublic(t, m, ip); got != "new.example" {
		t.Fatalf("stored public %q, want new.example: an address given must replace the one stored", got)
	}
}

// A worker first registered with no address stays that way rather than being
// an error or a surprise; workerPublicAddr falls back to the IP.
func TestAddWorkerToleratesNeverHavingAnAddress(t *testing.T) {
	const ip = "127.0.0.1"
	m := addWorkerTestManager(t)

	if err := m.AddWorker(ip, ""); err != nil {
		t.Fatalf("worker-add with no public address: %s", err)
	}
	if got := storedPublic(t, m, ip); got != "" {
		t.Fatalf("stored public %q, want empty", got)
	}
	if got := m.workerPublicAddr(ip); got != ip {
		t.Fatalf("workerPublicAddr = %q, want the ip %q as the fallback", got, ip)
	}
}
