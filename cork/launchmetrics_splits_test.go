package cork

import (
	"testing"
	"time"
)

// The container stage is where the launch spends most of its time, and until
// it is split there is no telling whether that is dockerd's create, dockerd's
// start, a port read-back sleeping on its own backoff, or the metadata write.
// These are the tests that keep the split honest: every value distinct, so a
// field wired to the wrong source fails instead of passing on a number that
// happens to match.

// The accumulator startContainers fills has to survive the trip into the
// sample. Without this the payload below could be perfect and every split
// still export a zero.
func TestContainerStageSplitsReachTheSample(t *testing.T) {
	m := newTestManager()
	m.workers = map[string]*workerConn{"10.0.0.1": {queue: newDaemonQueue(2)}}
	m.workerOrder = []string{"10.0.0.1"}
	timer := m.beginLaunch(
		&BuildMetadata{Id: 7, Challenge: "chal"},
		&InstanceMetadata{Id: 42, Worker: "10.0.0.1"},
		launchLimits{},
	)

	timer.containers(containerSplits{
		create:   11 * time.Millisecond,
		start:    22 * time.Millisecond,
		portRead: 33 * time.Millisecond,
		finalize: 44 * time.Millisecond,
		count:    3,
	})

	for _, c := range []struct {
		name string
		got  time.Duration
		want time.Duration
	}{
		{"ctrCreate", timer.sample.ctrCreate, 11 * time.Millisecond},
		{"ctrStart", timer.sample.ctrStart, 22 * time.Millisecond},
		{"portRead", timer.sample.portRead, 33 * time.Millisecond},
		{"finalize", timer.sample.finalize, 44 * time.Millisecond},
	} {
		if c.got != c.want {
			t.Errorf("sample.%s = %s, want %s", c.name, c.got, c.want)
		}
	}
	if timer.sample.ctrCount != 3 {
		t.Errorf("sample.ctrCount = %d, want 3", timer.sample.ctrCount)
	}
	if timer.sample.containers == 0 {
		t.Error("the stage total was not taken; the splits alone cannot show what is unattributed")
	}
}

// Each split must come from its own field. A copy-paste in payload that
// pointed two of them at the same source would otherwise export a plausible
// number and be invisible on a dashboard.
func TestEMFCarriesTheContainerStageSplits(t *testing.T) {
	event, _ := decodeEvent(t, testExporter(), launchSample{
		outcome: launchOK, worker: "10.0.0.1", at: time.Now(),
		total:      1500 * time.Millisecond,
		containers: 300 * time.Millisecond,
		ctrCreate:  11 * time.Millisecond,
		ctrStart:   22 * time.Millisecond,
		portRead:   33 * time.Millisecond,
		finalize:   44 * time.Millisecond,
		ctrCount:   3,
	})

	for name, want := range map[string]float64{
		"ContainerStart": 300,
		"DockerCreate":   11,
		"DockerStart":    22,
		"PortReadback":   33,
		"Finalize":       44,
		"ContainerCount": 3,
	} {
		got, ok := event[name].(float64)
		if !ok {
			t.Errorf("%s is missing or not a number: %v", name, event[name])
			continue
		}
		if got != want {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
}

// ContainerStart keeps meaning the whole stage. Redefining it as the start
// call alone would leave every panel already built on it plotting a smaller
// number with nothing to say it had changed.
func TestContainerStartStaysTheWholeStage(t *testing.T) {
	event, _ := decodeEvent(t, testExporter(), launchSample{
		outcome: launchOK, worker: "10.0.0.1", at: time.Now(),
		containers: 300 * time.Millisecond,
		ctrStart:   22 * time.Millisecond,
	})
	if got := event["ContainerStart"].(float64); got != 300 {
		t.Errorf("ContainerStart = %v, want the stage total 300", got)
	}
}

// The count is what the splits have to be read against -- a three-container
// challenge spends three creates -- but a field answers that for nothing,
// and a metric would be paid for every minute to say the same thing.
func TestContainerCountIsAFieldNotAMetric(t *testing.T) {
	event, _ := decodeEvent(t, testExporter(), launchSample{
		outcome: launchOK, worker: "10.0.0.1", at: time.Now(), ctrCount: 3,
	})
	for _, name := range metricNames(event) {
		if name == "ContainerCount" {
			t.Error("ContainerCount is published as a metric; it is context, and context goes in a field")
		}
	}
	if _, present := event["ContainerCount"]; !present {
		t.Error("ContainerCount is not in the record at all, so the splits cannot be normalised")
	}
}

// A launch that failed publishes no stage timings, and the splits are stage
// timings: a create that never returned is not a create time.
func TestAFailedLaunchPublishesNoSplits(t *testing.T) {
	event, _ := decodeEvent(t, testExporter(), launchSample{
		outcome: launchBusy, worker: "10.0.0.1", at: time.Now(),
		ctrCreate: 11 * time.Millisecond, ctrStart: 22 * time.Millisecond,
	})
	for _, name := range metricNames(event) {
		switch name {
		case "DockerCreate", "DockerStart", "PortReadback", "Finalize":
			t.Errorf("a refused launch published %q", name)
		}
	}
}
