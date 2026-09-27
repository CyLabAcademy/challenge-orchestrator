package cork

import (
	"errors"
	"testing"
	"time"
)

// A teardown is the one operation cork does that nobody waits on and nothing
// retries, which is exactly why it was invisible: no ring, no stage timings,
// and until now no record at all. These cover the two things that make it
// visible and the one that bounds it.

func teardownSample(outcome string, slotWait, removal time.Duration) launchSample {
	return launchSample{
		kind: sampleTeardown, outcome: outcome, worker: "10.0.0.1", workerIP: "10.0.0.1",
		instance: 42, slotWait: slotWait, total: removal, at: time.Now(),
		waiting: 3, teardownsWaiting: 5, slotsBusy: 2,
	}
}

// Every value distinct, so a field wired to the wrong source fails rather than
// matching by luck.
func TestEMFCarriesATeardown(t *testing.T) {
	event, _ := decodeEvent(t, testExporter(),
		teardownSample(launchOK, 1200*time.Millisecond, 340*time.Millisecond))

	for name, want := range map[string]float64{
		"TeardownFailed":   0,
		"TeardownWait":     1200,
		"TeardownDuration": 340,
		"Waiting":          3,
		"TeardownsWaiting": 5,
	} {
		got, ok := event[name].(float64)
		if !ok {
			t.Errorf("%s missing or not a number: %v", name, event[name])
			continue
		}
		if got != want {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
}

// A teardown record must not publish launch metrics: a stop is not a launch,
// and folding its duration into LaunchDuration would corrupt the number the
// whole export exists to report.
func TestATeardownPublishesNoLaunchMetrics(t *testing.T) {
	event, _ := decodeEvent(t, testExporter(),
		teardownSample(launchOK, 10*time.Millisecond, 80*time.Millisecond))
	for _, name := range metricNames(event) {
		switch name {
		case "LaunchDuration", "LaunchFailed", "ImageWait", "SlotWait",
			"NetworkCreate", "ContainerStart":
			t.Errorf("a teardown published the launch metric %q", name)
		}
	}
}

// TeardownWait is published even on a failure, unlike the duration. A stop
// that was refused because no slot came waited for its whole wait, and that
// wait is the measurement worth having -- it is the signal that a daemon has
// gone slow at removals while still answering its probes.
func TestARefusedTeardownStillPublishesItsWait(t *testing.T) {
	event, _ := decodeEvent(t, testExporter(),
		teardownSample(launchBusy, 10*time.Second, 0))
	got := map[string]bool{}
	for _, n := range metricNames(event) {
		got[n] = true
	}
	if !got["TeardownWait"] {
		t.Error("a refused teardown did not publish TeardownWait, which is the whole signal")
	}
	if !got["TeardownFailed"] {
		t.Error("a refused teardown did not publish TeardownFailed")
	}
	if got["TeardownDuration"] {
		t.Error("a refused teardown published a duration; nothing was removed")
	}
	if event["TeardownFailed"].(float64) != 1 {
		t.Error("TeardownFailed should be 1 on a refusal")
	}
}

// A launch carries the teardown queue's depth too. The two share a daemon but
// not a pool, so a launch that slowed while removals piled up is only
// diagnosable if both numbers are on the same record.
func TestALaunchCarriesTheTeardownQueueDepth(t *testing.T) {
	event, _ := decodeEvent(t, testExporter(), launchSample{
		outcome: launchOK, worker: "10.0.0.1", at: time.Now(),
		waiting: 2, teardownsWaiting: 9,
	})
	if got := event["Waiting"].(float64); got != 2 {
		t.Errorf("Waiting = %v, want 2", got)
	}
	if got := event["TeardownsWaiting"].(float64); got != 9 {
		t.Errorf("TeardownsWaiting = %v, want 9", got)
	}
}

// The bound exists so a stop cannot hold one of the platform's blocked workers
// forever against a daemon that is slow but still answering. Zero keeps the
// old behaviour for the rebuild path, which nothing retries.
func TestTeardownSlotWaitIsBounded(t *testing.T) {
	m := newTestManager()
	m.workersMu.Lock()
	q := newDaemonQueue(1)
	m.workers = map[string]*workerConn{"10.0.0.1": {queue: q}}
	m.workerOrder = []string{"10.0.0.1"}
	m.workersMu.Unlock()

	// Fill the single teardown slot and leave it held.
	q.teardownSem <- struct{}{}

	inst := &InstanceMetadata{Id: 7, Worker: "10.0.0.1"}
	start := time.Now()
	_, err := m.acquireSlot(q.teardownSem, inst, "teardown", 40*time.Millisecond)
	waited := time.Since(start)

	if err == nil {
		t.Fatal("a bounded wait on a full pool returned a slot")
	}
	if !errors.Is(err, ErrWorkerBusy) {
		t.Errorf("err = %v, want ErrWorkerBusy so the stop path can clear the records and succeed", err)
	}
	if waited > time.Second {
		t.Errorf("waited %s for a 40ms bound", waited)
	}
}

// The counter admit and the records read. Without it a deep teardown queue is
// invisible, which is the state this whole change exists to end.
func TestTeardownQueueDepthIsCounted(t *testing.T) {
	m := newTestManager()
	q := newDaemonQueue(1)
	m.workersMu.Lock()
	m.workers = map[string]*workerConn{"10.0.0.1": {queue: q}}
	m.workerOrder = []string{"10.0.0.1"}
	m.workersMu.Unlock()
	q.teardownSem <- struct{}{} // pool full

	inst := &InstanceMetadata{Id: 7, Worker: "10.0.0.1"}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = m.teardown(inst, 300*time.Millisecond)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if q.teardownWaiting.Load() == 1 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if got := q.teardownWaiting.Load(); got != 1 {
		t.Errorf("teardownWaiting = %d while a teardown was queued, want 1", got)
	}
	<-done
	if got := q.teardownWaiting.Load(); got != 0 {
		t.Errorf("teardownWaiting = %d after the teardown gave up, want 0", got)
	}
}

// A teardown is filed under the same name as a launch -- the player-facing one,
// permanent per EIP slot -- not the private IP. Under the IP the two operations
// land on different series, per-worker dashboards show no teardowns at all, and
// every ASG replacement mints a new billable dimension value.
func TestTeardownIsFiledUnderThePublicName(t *testing.T) {
	m := newTestManager()
	q := newDaemonQueue(1)
	m.workersMu.Lock()
	m.workers = map[string]*workerConn{"10.0.0.1": {queue: q, public: "worker-3.example"}}
	m.workerOrder = []string{"10.0.0.1"}
	m.workersMu.Unlock()
	// Through recordTeardown, not a hand-built sample: the naming lives there,
	// so a test that sets s.worker itself would pass with it removed.
	m.metrics = &launchMetrics{ch: make(chan launchSample, 1), log: newLogger(DISABLED)}

	inst := &InstanceMetadata{Id: 4, Worker: "10.0.0.1"}
	_, public := m.workerDaemon(inst)
	m.recordTeardown(queueDepth{}, public, inst, "", time.Second, 80*time.Millisecond, nil)

	select {
	case s := <-m.metrics.ch:
		if s.worker != "worker-3.example" {
			t.Errorf("sample.worker = %q, want the public name", s.worker)
		}
		if s.workerIP != "10.0.0.1" {
			t.Errorf("sample.workerIP = %q, want the private address", s.workerIP)
		}
	default:
		t.Fatal("recordTeardown filed nothing")
	}
}

// Every field is present on both kinds of record, so a Logs Insights query
// written against launches takes a zero per teardown and halves its own
// averages. Operation is what lets it filter.
func TestRecordsSayWhichOperationTheyAre(t *testing.T) {
	launch, _ := decodeEvent(t, testExporter(), launchSample{
		outcome: launchOK, worker: "w", at: time.Now(),
	})
	if launch["Operation"] != "launch" {
		t.Errorf("a launch record says Operation = %v, want launch", launch["Operation"])
	}
	down, _ := decodeEvent(t, testExporter(), teardownSample(launchOK, time.Second, time.Second))
	if down["Operation"] != "teardown" {
		t.Errorf("a teardown record says Operation = %v, want teardown", down["Operation"])
	}
}

// A rebuild's teardown waits as long as it takes, so its wait is not a signal
// that anything is wrong. Without a trigger to tell it apart, an alarm on
// TeardownWait fires on every ordinary update-schema.
func TestARebuildTeardownIsMarkedAsOne(t *testing.T) {
	down, _ := decodeEvent(t, testExporter(), launchSample{
		kind: sampleTeardown, outcome: launchOK, worker: "w",
		trigger: launchTriggerRestart, at: time.Now(),
	})
	if down["Trigger"] != launchTriggerRestart {
		t.Errorf("Trigger = %v, want %q so a rebuild's unbounded wait can be excluded", down["Trigger"], launchTriggerRestart)
	}
	stop, _ := decodeEvent(t, testExporter(), teardownSample(launchOK, time.Second, time.Second))
	if _, present := stop["Trigger"]; present {
		t.Errorf("a stop carried a Trigger (%v); only a rebuild should", stop["Trigger"])
	}
}

// A rebuild waits for its slot without a bound, on purpose, so its wait says
// nothing about the daemon's health. Trigger is a field and not a dimension,
// so a CloudWatch alarm cannot filter one out -- the only way to keep an
// update-schema from tripping an alarm on TeardownWait is to not publish the
// metric for it. The value stays on the record for a query to find.
func TestARebuildDoesNotPublishATeardownWaitMetric(t *testing.T) {
	rebuild, _ := decodeEvent(t, testExporter(), launchSample{
		kind: sampleTeardown, outcome: launchOK, worker: "w",
		trigger: launchTriggerRestart, slotWait: 90 * time.Second, at: time.Now(),
	})
	for _, name := range metricNames(rebuild) {
		if name == "TeardownWait" {
			t.Error("a rebuild published TeardownWait; an alarm on it would fire on every update-schema")
		}
	}
	if got, ok := rebuild["TeardownWait"].(float64); !ok || got != 90000 {
		t.Errorf("TeardownWait = %v, want it still present as a field for queries", rebuild["TeardownWait"])
	}

	// A stop's wait is the signal, and must still be published.
	stop, _ := decodeEvent(t, testExporter(), teardownSample(launchOK, 2*time.Second, time.Second))
	found := false
	for _, name := range metricNames(stop) {
		if name == "TeardownWait" {
			found = true
		}
	}
	if !found {
		t.Error("a stop did not publish TeardownWait, which is the metric worth alarming on")
	}
}

// The depths on a record have to be sampled where the operation was admitted,
// not where the record is filed: by then the removal has run and the slot has
// gone back, so a queue the teardown actually waited in has drained out of the
// number that is supposed to report it. A refusal against a full pool is the
// clean case -- the pool is still full when the record is made, and must be
// reported that way.
func TestARecordedDepthIsSampledAtAdmission(t *testing.T) {
	m := newTestManager()
	q := newDaemonQueue(1)
	m.workersMu.Lock()
	m.workers = map[string]*workerConn{"10.0.0.1": {queue: q, public: "w1"}}
	m.workerOrder = []string{"10.0.0.1"}
	m.workersMu.Unlock()
	m.metrics = &launchMetrics{ch: make(chan launchSample, 4), log: newLogger(DISABLED)}

	q.teardownSem <- struct{}{} // the only slot, held by someone else

	inst := &InstanceMetadata{Id: 11, Worker: "10.0.0.1"}
	if err := m.teardown(inst, 30*time.Millisecond); !errors.Is(err, ErrWorkerBusy) {
		t.Fatalf("teardown err = %v, want ErrWorkerBusy", err)
	}

	select {
	case s := <-m.metrics.ch:
		if s.slotsBusy != 1 {
			t.Errorf("slotsBusy = %d, want 1: the pool was full, which is why this teardown was refused", s.slotsBusy)
		}
	default:
		t.Fatal("a refused teardown filed no record")
	}
}
