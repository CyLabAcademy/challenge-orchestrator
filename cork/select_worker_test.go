package cork

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// managerWithWorkers builds a Manager holding the given workers in the order
// listed, which is also the round-robin order. Nothing here touches docker.
func managerWithWorkers(t *testing.T, workers ...*workerConn) *Manager {
	t.Helper()
	m := &Manager{log: newLogger(DISABLED)}
	m.workers = make(map[string]*workerConn, len(workers))
	for _, w := range workers {
		if _, dup := m.workers[w.ip]; dup {
			t.Fatalf("duplicate worker ip in fixture: %s", w.ip)
		}
		m.workers[w.ip] = w
		m.workerOrder = append(m.workerOrder, w.ip)
	}
	return m
}

// workerAt builds a worker fixture on both health axes. The three shorthands
// below cover what most placement tests care about.
func workerAt(ip string, r workerReachable, l workerLoad) *workerConn {
	w := &workerConn{ip: ip}
	w.reachable.Store(int32(r))
	w.load.Store(int32(l))
	return w
}

func okWorker(ip string) *workerConn { return workerAt(ip, workerReachableOk, workerLoadOk) }

// overloadedWorker is reachable -- its daemon answers, its telemetry says the
// box is over its high-water mark.
func overloadedWorker(ip string) *workerConn {
	return workerAt(ip, workerReachableOk, workerOverloaded)
}

// unresponsiveWorker had its daemon stop answering; its last known load is
// irrelevant, since reachability alone keeps it out of placement.
func unresponsiveWorker(ip string) *workerConn {
	return workerAt(ip, workerUnresponsive, workerLoadOk)
}

func downWorker(ip string) *workerConn { return workerAt(ip, workerDown, workerLoadOk) }

// No workers configured is not a failure: it means place the instance on the
// local daemon, which the caller reads as an empty ip with no error.
func TestSelectWorkerWithNoneConfigured(t *testing.T) {
	m := managerWithWorkers(t)

	ip, err := m.selectWorker()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ip != "" {
		t.Fatalf("got worker %q, want \"\" for local placement", ip)
	}
}

// Placement goes round the healthy workers in order and wraps, so a fleet
// shares the load instead of stacking every instance on the first box.
func TestSelectWorkerRoundRobins(t *testing.T) {
	m := managerWithWorkers(t,
		okWorker("10.0.0.1"),
		okWorker("10.0.0.2"),
		okWorker("10.0.0.3"),
	)

	want := []string{"10.0.0.1", "10.0.0.2", "10.0.0.3", "10.0.0.1", "10.0.0.2"}
	for i, expect := range want {
		got, err := m.selectWorker()
		if err != nil {
			t.Fatalf("placement %d: unexpected error: %v", i, err)
		}
		if got != expect {
			t.Fatalf("placement %d: got %s, want %s", i, got, expect)
		}
	}
}

// The cursor advances past what it skipped, not just past what it took, so a
// fleet with one healthy worker among unhealthy ones does not rescan the same
// dead prefix on every placement.
func TestSelectWorkerSkipsUnhealthy(t *testing.T) {
	m := managerWithWorkers(t,
		downWorker("10.0.0.1"),
		overloadedWorker("10.0.0.2"),
		unresponsiveWorker("10.0.0.4"),
		okWorker("10.0.0.3"),
	)

	for i := 0; i < 3; i++ {
		got, err := m.selectWorker()
		if err != nil {
			t.Fatalf("placement %d: unexpected error: %v", i, err)
		}
		if got != "10.0.0.3" {
			t.Fatalf("placement %d: got %s, want the one healthy worker 10.0.0.3", i, got)
		}
	}
}

// The three exhaustion errors mean different things to the caller. Overloaded
// and unresponsive are both states a worker leaves on its own, so they are
// retryable (503); every worker having been taken down by an operator is not
// going to resolve itself (500). See errorResponse in cmd/corkd, which maps
// exactly this distinction.
func TestSelectWorkerExhaustionErrors(t *testing.T) {
	tests := []struct {
		name    string
		workers []*workerConn
		want    error
	}{
		{
			name:    "every worker overloaded",
			workers: []*workerConn{overloadedWorker("10.0.0.1"), overloadedWorker("10.0.0.2")},
			want:    ErrAllWorkersOverloaded,
		},
		{
			name:    "every worker unresponsive",
			workers: []*workerConn{unresponsiveWorker("10.0.0.1"), unresponsiveWorker("10.0.0.2")},
			want:    ErrAllWorkersUnresponsive,
		},
		{
			name:    "every worker taken down",
			workers: []*workerConn{downWorker("10.0.0.1"), downWorker("10.0.0.2")},
			want:    ErrAllWorkersDown,
		},
		{
			// Unresponsive is a state a worker leaves on its own, so one
			// among the taken-down keeps the placement retryable.
			name:    "one unresponsive among the taken-down",
			workers: []*workerConn{downWorker("10.0.0.1"), unresponsiveWorker("10.0.0.2")},
			want:    ErrAllWorkersUnresponsive,
		},
		{
			// One overloaded among the dead is enough to make the whole
			// placement retryable: that box may come back on its own, where a
			// fleet that is entirely down needs someone to intervene.
			name:    "one overloaded among the down",
			workers: []*workerConn{downWorker("10.0.0.1"), overloadedWorker("10.0.0.2")},
			want:    ErrAllWorkersOverloaded,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := managerWithWorkers(t, tc.workers...)

			ip, err := m.selectWorker()
			if ip != "" {
				t.Fatalf("placed on %s despite no eligible worker", ip)
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

// The scan starts at the cursor and wraps, so one healthy worker anywhere in
// the order is always found however far round it sits. A fleet only refuses a
// placement when nothing is eligible.
func TestSelectWorkerWrapsToReachAHealthyWorker(t *testing.T) {
	first := okWorker("10.0.0.1")
	m := managerWithWorkers(t, first, overloadedWorker("10.0.0.2"))

	// Leaves the cursor on the overloaded worker.
	if got, err := m.selectWorker(); err != nil || got != "10.0.0.1" {
		t.Fatalf("first placement: got (%q, %v), want (10.0.0.1, nil)", got, err)
	}

	// The scan passes the overloaded worker, wraps, and comes back to the
	// healthy one rather than reporting the fleet exhausted.
	if got, err := m.selectWorker(); err != nil || got != "10.0.0.1" {
		t.Fatalf("second placement: got (%q, %v), want (10.0.0.1, nil)", got, err)
	}
}

// A refused placement must not move the cursor, or a fleet riding out a spell
// where everything is overloaded would walk the order while nothing is being
// placed, and resume somewhere arbitrary once it recovers.
func TestSelectWorkerCursorSurvivesExhaustion(t *testing.T) {
	one, two := okWorker("10.0.0.1"), okWorker("10.0.0.2")
	m := managerWithWorkers(t, one, two)

	// Takes the first worker and leaves the cursor on the second.
	if got, err := m.selectWorker(); err != nil || got != "10.0.0.1" {
		t.Fatalf("first placement: got (%q, %v), want (10.0.0.1, nil)", got, err)
	}

	one.load.Store(int32(workerOverloaded))
	two.load.Store(int32(workerOverloaded))
	for i := 0; i < 3; i++ {
		if _, err := m.selectWorker(); !errors.Is(err, ErrAllWorkersOverloaded) {
			t.Fatalf("refused placement %d: got %v, want ErrAllWorkersOverloaded", i, err)
		}
	}

	// Recovered: the cursor is where the last successful placement left it, so
	// the second worker is next rather than the first one over again.
	one.load.Store(int32(workerLoadOk))
	two.load.Store(int32(workerLoadOk))
	if got, err := m.selectWorker(); err != nil || got != "10.0.0.2" {
		t.Fatalf("after recovery: got (%q, %v), want (10.0.0.2, nil)", got, err)
	}
}

// The address handed to a player: the configured public one, else the private
// ip, and "" for a worker that has been removed since the instance was placed.
func TestWorkerPublicAddr(t *testing.T) {
	withPublic := okWorker("10.0.0.1")
	withPublic.public = "ctf.example.org"
	m := managerWithWorkers(t, withPublic, okWorker("10.0.0.2"))

	tests := []struct{ ip, want string }{
		{"10.0.0.1", "ctf.example.org"},
		{"10.0.0.2", "10.0.0.2"},
		{"10.0.0.9", ""},
	}
	for _, tc := range tests {
		if got := m.workerPublicAddr(tc.ip); got != tc.want {
			t.Fatalf("workerPublicAddr(%s): got %q, want %q", tc.ip, got, tc.want)
		}
	}
}

// The picoCTF platform reads the player-facing address under "hostname": with
// dynamic_public_hostname set it stores the whole launch response and looks that
// key up to resolve {{server}} in a challenge description. It is the same value
// as worker_public, so the two are asserted to agree here rather than being left
// to drift apart in a later edit.
func TestHostnameMirrorsWorkerPublic(t *testing.T) {
	withPublic := okWorker("10.0.0.1")
	withPublic.public = "ctf.example.org"
	m := managerWithWorkers(t, withPublic, okWorker("10.0.0.2"))

	for _, ip := range []string{"10.0.0.1", "10.0.0.2"} {
		meta := &InstanceMetadata{Id: 1, Worker: ip}
		meta.WorkerPublic = m.workerPublicAddr(meta.Worker)
		meta.Hostname = meta.WorkerPublic

		if meta.Hostname != meta.WorkerPublic {
			t.Fatalf("worker %s: hostname %q and worker_public %q disagree", ip, meta.Hostname, meta.WorkerPublic)
		}
		body, err := json.Marshal(meta)
		if err != nil {
			t.Fatalf("marshalling the instance: %s", err)
		}
		var out map[string]any
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("unmarshalling the instance: %s", err)
		}
		// Both keys, under the exact names each consumer reads.
		if out["hostname"] != meta.WorkerPublic {
			t.Errorf("worker %s: json 'hostname' is %v, want %q", ip, out["hostname"], meta.WorkerPublic)
		}
		if out["worker_public"] != meta.WorkerPublic {
			t.Errorf("worker %s: json 'worker_public' is %v, want %q", ip, out["worker_public"], meta.WorkerPublic)
		}
		if out["worker"] != ip {
			t.Errorf("worker %s: json 'worker' is %v, want the private address", ip, out["worker"])
		}
	}
}

// A local (single-host) instance has no worker, so neither address appears --
// omitempty keeps both keys out rather than reporting an empty hostname a
// consumer might template into a description.
func TestHostnameAbsentWithoutAWorker(t *testing.T) {
	body, err := json.Marshal(&InstanceMetadata{Id: 1})
	if err != nil {
		t.Fatalf("marshalling the instance: %s", err)
	}
	for _, key := range []string{"hostname", "worker_public", "worker"} {
		if strings.Contains(string(body), `"`+key+`"`) {
			t.Errorf("a workerless instance reported %q: %s", key, body)
		}
	}
}
