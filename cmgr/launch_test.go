package cmgr

import (
	"errors"
	"testing"
	"time"
)

func TestAcquireSlotBusyFailsFast(t *testing.T) {
	m := &Manager{log: newLogger(DISABLED)}
	sem := make(chan struct{}, 1)
	sem <- struct{}{} // held by another launch
	inst := &InstanceMetadata{Id: 7, Worker: ""}

	start := time.Now()
	_, err := m.acquireSlot(sem, inst, "launch", 30*time.Millisecond)
	if !errors.Is(err, ErrWorkerBusy) {
		t.Fatalf("expected ErrWorkerBusy, got %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("a full slot did not fail within the wait: %s", time.Since(start))
	}
	if len(sem) != 1 {
		t.Fatalf("the refused launch touched the slot: %d held", len(sem))
	}
}

func TestAcquireSlotRefusesDownWorker(t *testing.T) {
	m := &Manager{log: newLogger(DISABLED), workers: map[string]*workerConn{"10.0.0.1": testWorkerConn(workerDown)}}
	sem := make(chan struct{}, 1)
	inst := &InstanceMetadata{Id: 7, Worker: "10.0.0.1"}

	_, err := m.acquireSlot(sem, inst, "launch", time.Second)
	if !errors.Is(err, ErrWorkerDown) {
		t.Fatalf("expected ErrWorkerDown, got %v", err)
	}
	if len(sem) != 0 {
		t.Fatalf("a refused launch leaked a slot: %d held", len(sem))
	}
}

func TestAcquireSlotHoldsAndReleases(t *testing.T) {
	m := &Manager{log: newLogger(DISABLED), workers: map[string]*workerConn{"10.0.0.1": testWorkerConn(workerOk)}}
	sem := make(chan struct{}, 2)
	inst := &InstanceMetadata{Id: 7, Worker: "10.0.0.1"}

	release, err := m.acquireSlot(sem, inst, "launch", time.Second)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if len(sem) != 1 {
		t.Fatalf("slot not held: %d", len(sem))
	}
	release()
	if len(sem) != 0 {
		t.Fatalf("slot not released: %d", len(sem))
	}
}

// A zero wait (a restart during a rebuild) waits for the slot as long as it
// takes.
func TestAcquireSlotUnboundedWait(t *testing.T) {
	m := &Manager{log: newLogger(DISABLED)}
	sem := make(chan struct{}, 1)
	sem <- struct{}{} // held by another launch
	go func() {
		time.Sleep(50 * time.Millisecond)
		<-sem
	}()

	release, err := m.acquireSlot(sem, &InstanceMetadata{Id: 7}, "launch", 0)
	if err != nil {
		t.Fatalf("unbounded wait did not get the slot: %v", err)
	}
	release()
}

// The restart pull ceiling is minutes, or the configured pull timeout when
// that is longer; the worker client's transport timeout covers whichever
// per-call deadline is longest.
func TestRestartLimitsAndTransportTimeout(t *testing.T) {
	m := &Manager{log: newLogger(DISABLED)}
	m.workerTiming = m.workerTimingFromEnv()
	if got := m.restartLimits(); got.slotWait != 0 || got.pullTimeout != restartPullTimeout {
		t.Fatalf("default restart limits: %+v", got)
	}
	if got := m.requestLimits(); got.slotWait != defaultWorkerTiming.launchWait || got.pullTimeout != defaultWorkerTiming.pullTimeout {
		t.Fatalf("default request limits: %+v", got)
	}
	if got := m.transportTimeout(); got != restartPullTimeout {
		t.Fatalf("transport timeout %s, want the restart pull ceiling", got)
	}

	t.Setenv(WORKER_PULL_TIMEOUT_ENV, "10m")
	m.workerTiming = m.workerTimingFromEnv()
	if got := m.restartLimits().pullTimeout; got != 10*time.Minute {
		t.Fatalf("restart pull ceiling %s, want the longer configured pull timeout", got)
	}
	if got := m.transportTimeout(); got != 10*time.Minute {
		t.Fatalf("transport timeout %s, want the configured pull timeout", got)
	}

	t.Setenv(WORKER_CONTROL_TIMEOUT_ENV, "20m")
	m.workerTiming = m.workerTimingFromEnv()
	if got := m.transportTimeout(); got != 20*time.Minute {
		t.Fatalf("transport timeout %s, want the control timeout", got)
	}
}

// A launch queued for a slot gives up the moment its worker is marked down,
// instead of waiting out its launch wait against a daemon that will not come
// back: the platform's worker is freed for a retry placed elsewhere.
func TestAcquireSlotWakesOnWorkerDown(t *testing.T) {
	w := testWorkerConn(workerOk)
	w.downCh = make(chan struct{})
	m := &Manager{log: newLogger(DISABLED), workers: map[string]*workerConn{"10.0.0.1": w}}
	sem := make(chan struct{}, 1)
	sem <- struct{}{} // held by a launch that never finishes
	inst := &InstanceMetadata{Id: 7, Worker: "10.0.0.1"}

	go func() {
		time.Sleep(10 * time.Millisecond)
		m.markWorkerDown(w)
	}()
	start := time.Now()
	_, err := m.acquireSlot(sem, inst, "launch", time.Minute)
	if !errors.Is(err, ErrWorkerDown) {
		t.Fatalf("expected ErrWorkerDown, got %v", err)
	}
	if waited := time.Since(start); waited > 30*time.Second {
		t.Fatalf("the launch waited %s for a worker that had gone down", waited)
	}
	if len(sem) != 1 {
		t.Fatalf("the refused launch touched the slot: %d held", len(sem))
	}
}

// An instance on the local daemon has no worker to be down; no worker table
// is consulted.
func TestAcquireSlotLocalDaemon(t *testing.T) {
	m := &Manager{log: newLogger(DISABLED)}
	sem := make(chan struct{}, 1)
	release, err := m.acquireSlot(sem, &InstanceMetadata{Id: 1}, "launch", time.Second)
	if err != nil {
		t.Fatalf("acquire on the local daemon: %v", err)
	}
	release()
}
