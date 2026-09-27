package cork

import (
	"context"
	"errors"
	"testing"
	"time"
)

// The bound on a teardown slot is only safe because a refusal changes nothing
// else. stopInstance forgets an instance whose removal was already sent (a
// control timeout: dockerd finishes what it has been given) and one whose
// worker has left placement (reconcileWorker clears it on the way back). A
// refusal is neither: nothing reached the daemon and the worker stays
// placeable, so the containers are still there holding their ports. Freeing
// those rows would hand the same ports to the next launch placed on that
// worker, which would then fail on the bind -- and the deployed reaper only
// collects containers labelled cmgr.dynamic=true, so a static one would never
// be cleaned up at all.
//
// These are the tests that keep that distinction, because the failure it
// prevents is silent: the ports look free, and the launches that fail are
// somebody else's.

// A refused teardown must surface ErrWorkerBusy, which is what tells
// stopInstance not to treat it like a sent-but-slow removal.
func TestARefusedTeardownReportsBusyNotSuccess(t *testing.T) {
	m := newTestManager()
	q := newDaemonQueue(1)
	m.workersMu.Lock()
	m.workers = map[string]*workerConn{"10.0.0.1": {queue: q}}
	m.workerOrder = []string{"10.0.0.1"}
	m.workersMu.Unlock()
	q.teardownSem <- struct{}{} // the only slot, held

	err := m.teardown(&InstanceMetadata{Id: 7, Worker: "10.0.0.1"}, 30*time.Millisecond)
	if err == nil {
		t.Fatal("a teardown that never got a slot reported success")
	}
	if !errors.Is(err, ErrWorkerBusy) {
		t.Errorf("err = %v, want ErrWorkerBusy", err)
	}
	// Not the error that means "already sent, dockerd will finish it".
	if errors.Is(err, context.DeadlineExceeded) {
		t.Error("a refusal must not look like a deadline: stopInstance forgets those")
	}
}

// A refused stop keeps the instance and its port reservations. The port is the
// part that matters: cork hands out ports from what the database says is free,
// so a row deleted while its container lives is a port collision waiting for
// the next launch on that worker.
func TestARefusedStopKeepsTheInstanceAndItsPorts(t *testing.T) {
	const portLow, portHigh = 31000, 31063
	m := setupPortAssignments(t, portLow, portHigh, map[string]int{"challenge": 31000})

	var before int
	if err := m.db.Get(&before, "SELECT COUNT(*) FROM portAssignments;"); err != nil {
		t.Fatal(err)
	}
	if before == 0 {
		t.Fatal("the fixture recorded no port assignment, so this proves nothing")
	}
	var instID InstanceId
	if err := m.db.Get(&instID, "SELECT id FROM instances LIMIT 1;"); err != nil {
		t.Fatal(err)
	}

	// A worker whose only teardown slot is taken, so the stop is refused.
	q := newDaemonQueue(1)
	m.workersMu.Lock()
	m.workers = map[string]*workerConn{"10.0.0.9": {queue: q}}
	m.workerOrder = []string{"10.0.0.9"}
	m.workersMu.Unlock()
	q.teardownSem <- struct{}{}

	if _, err := m.db.Exec("UPDATE instances SET worker = ? WHERE id = ?;", "10.0.0.9", instID); err != nil {
		t.Fatal(err)
	}
	inst := &InstanceMetadata{Id: instID, Worker: "10.0.0.9"}

	if err := m.teardown(inst, 30*time.Millisecond); !errors.Is(err, ErrWorkerBusy) {
		t.Fatalf("teardown err = %v, want ErrWorkerBusy", err)
	}

	var after int
	if err := m.db.Get(&after, "SELECT COUNT(*) FROM portAssignments;"); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Errorf("port assignments went %d -> %d after a refused stop; those ports are still held by a live container", before, after)
	}
	var instances int
	if err := m.db.Get(&instances, "SELECT COUNT(*) FROM instances WHERE id = ?;", instID); err != nil {
		t.Fatal(err)
	}
	if instances != 1 {
		t.Error("a refused stop dropped the instance row; nothing then accounts for its containers")
	}
}
