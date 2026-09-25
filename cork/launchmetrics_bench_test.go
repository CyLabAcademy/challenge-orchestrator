package cork

import (
	"encoding/json"
	"net"
	"testing"
	"time"
)

// What the instrumentation costs the goroutine that is launching a container.
// The question these answer is whether any of this belongs off the launch
// path: a launch takes seconds, so anything here in the nanoseconds is free,
// and the figures are in the commit message.
//
// benchLaunch runs exactly what launchStages runs -- beginLaunch, the four
// stage splits, and finish -- so nothing is left out of the measurement.
func benchLaunch(b *testing.B, m *Manager) {
	// A real worker with a real queue, so the measurement includes the
	// workers lock and the ring, as a launch on a fleet does.
	m.workers = map[string]*workerConn{"10.0.0.1": {queue: newDaemonQueue(2)}}
	m.workerOrder = []string{"10.0.0.1"}
	build := &BuildMetadata{Id: 7, Challenge: "bench/chal"}
	instance := &InstanceMetadata{Id: 42, Worker: "10.0.0.1"}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		t := m.beginLaunch(build, instance, launchLimits{})
		t.images(false)
		t.slot()
		t.network()
		t.containers()
		t.finish(nil)
	}
}

// The deployed configuration: the ring, plus a sample handed to a live
// emitter over the channel.
func BenchmarkLaunchInstrumentationWithExport(b *testing.B) {
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		b.Fatal(err)
	}
	defer conn.Close()
	// Drain, so the emitter is writing into something that reads, as the
	// CloudWatch agent would.
	go func() {
		buf := make([]byte, 8192)
		for {
			if _, _, err := conn.ReadFrom(buf); err != nil {
				return
			}
		}
	}()

	lm := &launchMetrics{
		ch:        make(chan launchSample, emfQueueDepth),
		endpoint:  conn.LocalAddr().String(),
		logGroup:  defaultEMFLogGroup,
		namespace: defaultEMFNamespace,
		log:       newLogger(DISABLED),
	}
	go lm.run()
	defer close(lm.ch)

	benchLaunch(b, &Manager{metrics: lm})
}

// Everything but the export: what a deployment with no CloudWatch agent pays,
// which is also what the e2e and the test VM pay.
func BenchmarkLaunchInstrumentationRingOnly(b *testing.B) {
	benchLaunch(b, &Manager{})
}

// The ring on its own, which is the only part that takes a lock on the launch
// path.
func BenchmarkLaunchRingAdd(b *testing.B) {
	var r launchRing
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.add(time.Duration(i)*time.Millisecond, true)
	}
}

// Contended, because launches on one worker run concurrently and this is the
// one place they meet.
func BenchmarkLaunchRingAddParallel(b *testing.B) {
	var r launchRing
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			r.add(time.Millisecond, true)
		}
	})
}

// The worker-list path: a sort of the ring, per worker, per request. Not on
// the launch path at all, but worth knowing before it is called from
// something that polls.
func BenchmarkLaunchRingStats(b *testing.B) {
	var r launchRing
	for i := 0; i < launchRingSize; i++ {
		r.add(time.Duration(i)*time.Millisecond, true)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = r.stats()
	}
}

// The emitter goroutine's own cost: the marshal and the datagram. This is off
// the launch path -- it bounds how many launches a second the exporter can
// keep up with before the queue starts dropping.
func BenchmarkEMFEmit(b *testing.B) {
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		b.Fatal(err)
	}
	defer conn.Close()
	go func() {
		buf := make([]byte, 8192)
		for {
			if _, _, err := conn.ReadFrom(buf); err != nil {
				return
			}
		}
	}()

	lm := testExporter()
	lm.endpoint = conn.LocalAddr().String()
	s := launchSample{
		outcome: launchOK, worker: "10.0.0.1", challenge: "bench/chal",
		build: 7, instance: 42, total: 1500 * time.Millisecond,
		imageWait: 900 * time.Millisecond, slotWait: 100 * time.Millisecond,
		netCreate: 200 * time.Millisecond, containers: 300 * time.Millisecond,
		waiting: 3, slotsBusy: 2, at: time.Now(),
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		lm.emit(s)
	}
}

// The size of one record, which is what the CloudWatch Logs bill is a
// function of, and the bound the agent enforces (a log event may not exceed
// 1 MB). Logged rather than pinned to an exact number: the figure is a fact
// to know when sizing the bill, not a contract.
func TestEMFPayloadSize(t *testing.T) {
	lm := testExporter()
	s := launchSample{
		outcome: launchOK, worker: "10.0.0.1", challenge: "bench/chal",
		build: 7, instance: 42, total: 1500 * time.Millisecond,
		imageWait: 900 * time.Millisecond, slotWait: 100 * time.Millisecond,
		netCreate: 200 * time.Millisecond, containers: 300 * time.Millisecond,
		waiting: 3, slotsBusy: 2, at: time.Now(),
	}
	raw, err := json.Marshal(lm.payload(s))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("one record is %d bytes; %.0f MB a month at 5,000 launches a day",
		len(raw), float64(len(raw))*5000*30/1e6)
	if len(raw) > 4096 {
		t.Errorf("a record grew to %d bytes; the export was sized on a few hundred", len(raw))
	}
}
