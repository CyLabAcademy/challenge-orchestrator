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

// A launch waits only for the launches ahead of it beyond the slots: nothing
// is estimated while a slot is free, nor before a hold time has been
// measured, and the estimate follows the latest samples.
func TestDaemonQueueExpectedWait(t *testing.T) {
	q := newDaemonQueue(2)
	if expected, _ := q.expectedWait(); expected != 0 {
		t.Fatalf("an estimate before any measurement: %s", expected)
	}
	q.recordHold(200 * time.Millisecond)
	if expected, _ := q.expectedWait(); expected != 0 {
		t.Fatalf("expected wait on an idle daemon = %s, want 0", expected)
	}
	q.launchSem <- struct{}{} // one launch in progress, one slot still free
	if expected, _ := q.expectedWait(); expected != 0 {
		t.Fatalf("expected wait with a free slot = %s, want 0", expected)
	}
	q.launchSem <- struct{}{} // both slots taken, nobody queued
	if expected, _ := q.expectedWait(); expected != 100*time.Millisecond {
		t.Fatalf("expected wait behind two launches on 2 slots = %s, want half a hold", expected)
	}
	q.waiting.Store(98) // 100 ahead: 99 of them must finish first
	if expected, waiting := q.expectedWait(); expected != 9900*time.Millisecond || waiting != 98 {
		t.Fatalf("expected wait behind 98 waiting and 2 holding on 2 slots = %s (%d waiting), want 9.9s", expected, waiting)
	}
	<-q.launchSem
	<-q.launchSem
	q.waiting.Store(0)
	q.recordHold(time.Second)
	if hold := time.Duration(q.holdNanos.Load()); hold != 400*time.Millisecond {
		t.Fatalf("hold estimate after a 1s sample = %s, want 400ms (a quarter of the new sample)", hold)
	}
	var none *daemonQueue
	if expected, waiting := none.expectedWait(); expected != 0 || waiting != 0 {
		t.Fatalf("a nil queue estimated %s, %d", expected, waiting)
	}
}

// A slow spell that pushed the hold estimate past the launch wait must not
// keep refusing launches once the daemon is idle: the next launch is admitted
// and its sample brings the estimate down.
func TestAdmitRecoversFromAnInflatedEstimate(t *testing.T) {
	m := &Manager{log: newLogger(DISABLED), localQueue: newDaemonQueue(2)}
	inst := &InstanceMetadata{}
	limits := launchLimits{slotWait: 10 * time.Second}
	q := m.localQueue

	q.recordHold(40 * time.Second) // holds ran long while the daemon was slow
	if err := m.admit(inst, limits); err != nil {
		t.Fatalf("an idle daemon with an inflated estimate refused a launch: %v", err)
	}
	q.launchSem <- struct{}{}
	if err := m.admit(inst, limits); err != nil {
		t.Fatalf("a launch with a slot free beside it was refused: %v", err)
	}
	q.launchSem <- struct{}{} // both slots taken: the next one waits out a hold
	if err := m.admit(inst, limits); !errors.Is(err, ErrWorkerBusy) {
		t.Fatalf("a launch behind a 20s expected wait was admitted: %v", err)
	}
	<-q.launchSem
	<-q.launchSem
	for i := 0; i < 8; i++ {
		q.recordHold(time.Second) // the daemon is back to its pace
	}
	q.launchSem <- struct{}{}
	q.launchSem <- struct{}{}
	if err := m.admit(inst, limits); err != nil {
		t.Fatalf("the estimate did not recover after eight fast launches: %v", err)
	}
	<-q.launchSem
	<-q.launchSem
}

// A launch whose expected wait exceeds its launch wait is refused as busy
// before anything is recorded; one within it, or one with an unbounded
// wait, is admitted.
func TestAdmitRefusesABacklog(t *testing.T) {
	m := &Manager{log: newLogger(DISABLED), localQueue: newDaemonQueue(2)}
	inst := &InstanceMetadata{}
	limits := launchLimits{slotWait: 10 * time.Second}

	m.localQueue.waiting.Store(103)
	if err := m.admit(inst, limits); err != nil {
		t.Fatalf("refused before any hold time was measured: %v", err)
	}
	m.localQueue.recordHold(200 * time.Millisecond)
	if err := m.admit(inst, limits); !errors.Is(err, ErrWorkerBusy) {
		t.Fatalf("a 10.2s expected wait against a 10s launch wait was admitted: %v", err)
	}
	if err := m.admit(inst, launchLimits{slotWait: 0}); err != nil {
		t.Fatalf("an unbounded wait was refused: %v", err)
	}
	m.localQueue.waiting.Store(50)
	if err := m.admit(inst, limits); err != nil {
		t.Fatalf("a 4.9s expected wait against a 10s launch wait was refused: %v", err)
	}
}

// The waiting count covers the wait only, and the hold time is recorded on
// release.
func TestAcquireLaunchSlotAccounting(t *testing.T) {
	m := &Manager{log: newLogger(DISABLED)}
	q := newDaemonQueue(1)
	inst := &InstanceMetadata{Id: 7}

	q.launchSem <- struct{}{} // held by another launch
	if _, err := m.acquireLaunchSlot(q, inst, 20*time.Millisecond); !errors.Is(err, ErrWorkerBusy) {
		t.Fatalf("expected ErrWorkerBusy, got %v", err)
	}
	if q.waiting.Load() != 0 {
		t.Fatalf("a refused launch is still counted as waiting: %d", q.waiting.Load())
	}
	<-q.launchSem

	release, err := m.acquireLaunchSlot(q, inst, time.Second)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if q.waiting.Load() != 0 {
		t.Fatalf("a launch holding its slot is counted as waiting: %d", q.waiting.Load())
	}
	time.Sleep(5 * time.Millisecond)
	release()
	if len(q.launchSem) != 0 {
		t.Fatal("slot not released")
	}
	if q.holdNanos.Load() < int64(5*time.Millisecond) {
		t.Fatalf("hold time not recorded on release: %s", time.Duration(q.holdNanos.Load()))
	}
}
