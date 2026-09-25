package cork

import (
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

func TestQuantileIndexIsNearestRank(t *testing.T) {
	// Nearest rank: the lowest sample at or above the percentile, so every
	// answer is a value that was actually measured.
	cases := []struct{ n, pct, want int }{
		{1, 50, 0}, {1, 90, 0},
		{2, 50, 0}, {2, 90, 1},
		{10, 50, 4}, {10, 90, 8}, {10, 100, 9},
		{100, 50, 49}, {100, 90, 89},
		{256, 50, 127}, {256, 90, 230},
	}
	for _, c := range cases {
		if got := quantileIndex(c.n, c.pct); got != c.want {
			t.Errorf("quantileIndex(%d, %d) = %d, want %d", c.n, c.pct, got, c.want)
		}
	}
}

func TestQuantileIndexStaysInRange(t *testing.T) {
	for n := 1; n <= 300; n++ {
		for _, pct := range []int{0, 1, 50, 90, 99, 100} {
			if i := quantileIndex(n, pct); i < 0 || i >= n {
				t.Fatalf("quantileIndex(%d, %d) = %d, out of range", n, pct, i)
			}
		}
	}
}

func TestLaunchRingEmpty(t *testing.T) {
	var r launchRing
	if got := r.stats(); got != (LaunchStats{}) {
		t.Errorf("empty ring gave %+v, want the zero value", got)
	}
}

func TestLaunchRingSummarizes(t *testing.T) {
	var r launchRing
	for i := 1; i <= 10; i++ {
		r.add(time.Duration(i)*100*time.Millisecond, true)
	}
	got := r.stats()
	want := LaunchStats{Count: 10, Samples: 10, P50Ms: 500, P90Ms: 900, MaxMs: 1000}
	if got != want {
		t.Errorf("stats = %+v, want %+v", got, want)
	}
}

// A failed launch is counted but contributes no duration: a refusal that took
// three milliseconds would otherwise drag every quantile down and make a
// daemon turning work away look like the fastest in the fleet.
func TestLaunchRingCountsFailuresWithoutTiming(t *testing.T) {
	var r launchRing
	for i := 0; i < 4; i++ {
		r.add(time.Second, true)
	}
	for i := 0; i < 6; i++ {
		r.add(3*time.Millisecond, false)
	}
	got := r.stats()
	want := LaunchStats{Count: 10, Failed: 6, Samples: 4, P50Ms: 1000, P90Ms: 1000, MaxMs: 1000}
	if got != want {
		t.Errorf("stats = %+v, want %+v", got, want)
	}
}

// The case the count exists for: a worker refusing everything must not look
// like a worker nothing is being sent to.
func TestLaunchRingShowsAWorkerFailingEverything(t *testing.T) {
	var r launchRing
	for i := 0; i < 25; i++ {
		r.add(3*time.Millisecond, false)
	}
	got := r.stats()
	if got.Count != 25 || got.Failed != 25 {
		t.Errorf("Count = %d, Failed = %d; want 25 and 25", got.Count, got.Failed)
	}
	if got.Samples != 0 {
		t.Errorf("Samples = %d, want 0: no launch succeeded", got.Samples)
	}
	if got.MaxMs != 0 {
		t.Errorf("MaxMs = %d, want 0: a failure contributes no duration", got.MaxMs)
	}
}

// The ring is the last launchRingSize launches, but the count is every launch
// the daemon has taken: a worker that has been running all day should not
// report that it has served 256 instances.
func TestLaunchRingKeepsTotalButWindowsQuantiles(t *testing.T) {
	var r launchRing
	// A slow spell, then enough fast launches to push it entirely out.
	for i := 0; i < launchRingSize; i++ {
		r.add(60*time.Second, true)
	}
	for i := 0; i < launchRingSize; i++ {
		r.add(200*time.Millisecond, true)
	}
	got := r.stats()
	if got.Count != 2*launchRingSize {
		t.Errorf("Count = %d, want %d: the total is not capped by the ring", got.Count, 2*launchRingSize)
	}
	if got.MaxMs != 200 {
		t.Errorf("MaxMs = %d, want 200: the slow spell should have aged out of the window", got.MaxMs)
	}
}

func TestLaunchRingWrapsWithoutLosingOrder(t *testing.T) {
	var r launchRing
	// One more than the ring holds: the first sample is overwritten, and the
	// quantiles must come from the survivors rather than from a zero left
	// behind by the wrap.
	for i := 0; i <= launchRingSize; i++ {
		r.add(time.Duration(i+1)*time.Millisecond, true)
	}
	got := r.stats()
	if got.MaxMs != int64(launchRingSize+1) {
		t.Errorf("MaxMs = %d, want %d", got.MaxMs, launchRingSize+1)
	}
	if got.P50Ms == 0 {
		t.Error("P50Ms = 0: a wrapped ring is reading slots it never wrote")
	}
}

func TestLaunchOutcomeOf(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{nil, launchOK},
		{ErrWorkerBusy, launchBusy},
		{ErrWorkerUnreachable, launchUnreachable},
		{ErrPullTimeout, launchPullTimeout},
		{fmt.Errorf("something else"), launchFailed},
		// Wrapped, which is how launch.go actually returns them.
		{fmt.Errorf("instance 4: %w", ErrWorkerBusy), launchBusy},
		{fmt.Errorf("instance 4: %w", ErrPullTimeout), launchPullTimeout},
	}
	for _, c := range cases {
		if got := launchOutcomeOf(c.err); got != c.want {
			t.Errorf("launchOutcomeOf(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}

// A nil exporter is the configuration every deployment without a CloudWatch
// agent runs, so it has to be a working no-op rather than a panic.
func TestNilLaunchMetricsRecords(t *testing.T) {
	var lm *launchMetrics
	lm.record(launchSample{total: time.Second})
}

// record must never block, because it is called from the launch goroutine. A
// full queue drops and counts rather than waiting for the emitter.
func TestRecordDropsRatherThanBlocking(t *testing.T) {
	lm := &launchMetrics{ch: make(chan launchSample, 2), log: newLogger(DISABLED)}
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			lm.record(launchSample{outcome: launchOK})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("record blocked on a full queue")
	}
	if got := lm.dropped.Load(); got != 98 {
		t.Errorf("dropped = %d, want 98", got)
	}
}

// ---------------------------------------------------------------------------
// Embedded metric format
// ---------------------------------------------------------------------------

func testExporter() *launchMetrics {
	return &launchMetrics{
		logGroup:  "cork",
		namespace: "cork",
		log:       newLogger(DISABLED),
	}
}

// decodeEvent marshals a payload the way emit does and reads it back as a
// bare map, so the assertions below see exactly what CloudWatch would.
func decodeEvent(t *testing.T, lm *launchMetrics, s launchSample) (map[string]any, []byte) {
	t.Helper()
	raw, err := json.Marshal(lm.payload(s))
	if err != nil {
		t.Fatalf("marshalling the payload: %s", err)
	}
	var event map[string]any
	if err := json.Unmarshal(raw, &event); err != nil {
		t.Fatalf("the payload is not valid JSON: %s", err)
	}
	return event, raw
}

// The agent rejects an event that spans lines, and json.Marshal is only
// newline-free as long as nobody reaches for MarshalIndent.
func TestEMFEventIsOneLine(t *testing.T) {
	_, raw := decodeEvent(t, testExporter(), launchSample{outcome: launchOK, at: time.Now()})
	if strings.ContainsAny(string(raw), "\n\r") {
		t.Errorf("the payload spans lines, which the agent rejects:\n%s", raw)
	}
}

// The central rule of the spec: every name a directive lists, as a metric or
// as a dimension, must exist as a member of the root node. An event that
// breaks it is dropped by CloudWatch with nothing said on the cork side, so
// this is the check that would catch a silent hole in the metrics.
func TestEMFTargetsResolveToRootMembers(t *testing.T) {
	samples := map[string]launchSample{
		"success": {outcome: launchOK, worker: "10.0.0.1", total: 1500 * time.Millisecond, at: time.Now()},
		"failure": {outcome: launchBusy, worker: "10.0.0.1", total: 3 * time.Millisecond, at: time.Now()},
		"local":   {outcome: launchOK, worker: "", total: time.Second, at: time.Now()},
	}
	for name, s := range samples {
		t.Run(name, func(t *testing.T) {
			event, _ := decodeEvent(t, testExporter(), s)

			meta, ok := event["_aws"].(map[string]any)
			if !ok {
				t.Fatal("_aws is missing")
			}
			if _, ok := meta["Timestamp"].(float64); !ok {
				t.Error("_aws.Timestamp is missing or not a number")
			}
			if meta["LogGroupName"] != "cork" {
				t.Errorf("_aws.LogGroupName = %v, want cork; the agent needs it to file the event", meta["LogGroupName"])
			}

			directives, ok := meta["CloudWatchMetrics"].([]any)
			if !ok || len(directives) == 0 {
				t.Fatal("_aws.CloudWatchMetrics is missing or empty")
			}
			for _, d := range directives {
				dir := d.(map[string]any)
				if dir["Namespace"] != "cork" {
					t.Errorf("Namespace = %v, want cork", dir["Namespace"])
				}
				for _, m := range dir["Metrics"].([]any) {
					metric := m.(map[string]any)
					n := metric["Name"].(string)
					if _, present := event[n]; !present {
						t.Errorf("metric %q names no root member", n)
					}
					if _, numeric := event[n].(float64); !numeric {
						t.Errorf("metric %q is not a number at the root", n)
					}
				}
				for _, set := range dir["Dimensions"].([]any) {
					for _, key := range set.([]any) {
						k := key.(string)
						v, present := event[k]
						if !present {
							t.Errorf("dimension %q names no root member", k)
							continue
						}
						str, isString := v.(string)
						if !isString || str == "" {
							t.Errorf("dimension %q is %v; a dimension value must be a non-empty string", k, v)
						}
					}
				}
			}
		})
	}
}

// A launch that was refused after three milliseconds is not a launch time.
// Publishing it would drag every quantile down and make a daemon that is
// turning work away look like the fastest in the fleet.
func TestEMFFailedLaunchPublishesNoDuration(t *testing.T) {
	event, _ := decodeEvent(t, testExporter(), launchSample{
		outcome: launchBusy, worker: "10.0.0.1", total: 3 * time.Millisecond, at: time.Now(),
	})
	for _, name := range metricNames(event) {
		if name != "LaunchFailed" {
			t.Errorf("a refused launch published %q; only LaunchFailed belongs on a failure", name)
		}
	}
	if event["LaunchFailed"].(float64) != 1 {
		t.Error("LaunchFailed should be 1 on a refusal")
	}
	if event["Outcome"] != launchBusy {
		t.Errorf("Outcome = %v, want %q", event["Outcome"], launchBusy)
	}
}

// LaunchFailed is published on success too, as a zero. Without that there is
// no denominator, and a failure rate cannot be computed from a metric that
// only exists when things go wrong.
func TestEMFSuccessPublishesTheFullSet(t *testing.T) {
	event, _ := decodeEvent(t, testExporter(), launchSample{
		outcome: launchOK, worker: "10.0.0.1", total: 1500 * time.Millisecond,
		imageWait: 900 * time.Millisecond, slotWait: 100 * time.Millisecond,
		netCreate: 200 * time.Millisecond, containers: 300 * time.Millisecond,
		at: time.Now(),
	})
	got := map[string]bool{}
	for _, n := range metricNames(event) {
		got[n] = true
	}
	for _, want := range []string{"LaunchFailed", "LaunchDuration", "ImageWait", "SlotWait", "NetworkCreate", "ContainerStart"} {
		if !got[want] {
			t.Errorf("a successful launch did not publish %q", want)
		}
	}
	if event["LaunchFailed"].(float64) != 0 {
		t.Error("LaunchFailed should be 0 on success, so that a failure rate has a denominator")
	}
	if event["LaunchDuration"].(float64) != 1500 {
		t.Errorf("LaunchDuration = %v, want 1500", event["LaunchDuration"])
	}
}

// An instance on the orchestrator's own daemon has no worker name, and an
// empty dimension value is not a dimension.
func TestEMFLocalDaemonGetsADimensionValue(t *testing.T) {
	event, _ := decodeEvent(t, testExporter(), launchSample{outcome: launchOK, worker: "", at: time.Now()})
	if event["Worker"] != "local" {
		t.Errorf("Worker = %v, want local", event["Worker"])
	}
}

// The context fields are the point of the per-launch record: a duration on
// its own cannot be diagnosed.
func TestEMFCarriesTheDaemonContext(t *testing.T) {
	event, _ := decodeEvent(t, testExporter(), launchSample{
		outcome: launchOK, worker: "10.0.0.1", challenge: "test/chal", build: 7, instance: 42,
		waiting: 5, slotsBusy: 2, pulled: true, at: time.Now(),
	})
	for key, want := range map[string]any{
		"Challenge": "test/chal", "Build": float64(7), "Instance": float64(42),
		"Waiting": float64(5), "SlotsBusy": float64(2), "ImagePulled": true,
	} {
		if event[key] != want {
			t.Errorf("%s = %v, want %v", key, event[key], want)
		}
	}
}

func metricNames(event map[string]any) []string {
	var names []string
	meta := event["_aws"].(map[string]any)
	for _, d := range meta["CloudWatchMetrics"].([]any) {
		for _, m := range d.(map[string]any)["Metrics"].([]any) {
			names = append(names, m.(map[string]any)["Name"].(string))
		}
	}
	return names
}

// End to end over a real socket, which is the only way to see that emit
// dials, writes a single datagram, and counts it.
func TestEmitWritesOneDatagram(t *testing.T) {
	addr, err := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	lm := testExporter()
	lm.endpoint = conn.LocalAddr().String()

	lm.emit(launchSample{outcome: launchOK, worker: "10.0.0.1", total: 1200 * time.Millisecond, at: time.Now()})

	if got := lm.sent.Load(); got != 1 {
		t.Fatalf("sent = %d, want 1", got)
	}
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 4096)
	n, _, err := conn.ReadFrom(buf)
	if err != nil {
		t.Fatalf("reading the datagram: %s", err)
	}
	var event map[string]any
	if err := json.Unmarshal(buf[:n], &event); err != nil {
		t.Fatalf("the datagram is not valid JSON: %s\n%s", err, buf[:n])
	}
	if event["LaunchDuration"].(float64) != 1200 {
		t.Errorf("LaunchDuration = %v, want 1200", event["LaunchDuration"])
	}
}

// With nothing listening, emit must give up quietly and back off rather than
// dialing once per launch. A connected UDP socket reports the ICMP refusal of
// the previous write, so the failure surfaces on the second write.
func TestEmitBacksOffWhenNothingIsListening(t *testing.T) {
	lm := testExporter()
	// Port 1 on the loopback: reserved, and nothing binds it.
	lm.endpoint = "127.0.0.1:1"
	for i := 0; i < 5; i++ {
		lm.emit(launchSample{outcome: launchOK, at: time.Now()})
	}
	if lm.nextDial.IsZero() {
		t.Skip("this platform accepted every write to a closed port; nothing to back off from")
	}
	if !lm.refused {
		t.Error("a refused endpoint should be recorded, so it is reported once rather than per launch")
	}
	if lm.conn != nil {
		t.Error("a failed socket should be closed, not kept")
	}
}

func TestNewLaunchMetricsIsOffWithoutAnEndpoint(t *testing.T) {
	t.Setenv(EMF_ENDPOINT_ENV, "")
	t.Setenv(legacyEnvName(EMF_ENDPOINT_ENV), "")
	if lm := newLaunchMetrics(newLogger(DISABLED)); lm != nil {
		t.Error("the exporter should be nil when no endpoint is configured")
	}
}

func TestNewLaunchMetricsDefaultsGroupAndNamespace(t *testing.T) {
	t.Setenv(EMF_ENDPOINT_ENV, "127.0.0.1:25888")
	lm := newLaunchMetrics(newLogger(DISABLED))
	if lm == nil {
		t.Fatal("the exporter should exist when an endpoint is configured")
	}
	if lm.logGroup != defaultEMFLogGroup || lm.namespace != defaultEMFNamespace {
		t.Errorf("log group %q, namespace %q; want the defaults %q and %q",
			lm.logGroup, lm.namespace, defaultEMFLogGroup, defaultEMFNamespace)
	}
}

// A connected UDP socket reports a refusal on the write after the one that
// drew it, so the first write following a redial succeeds even against a dead
// endpoint. Treating that as recovery is what produced an endless pair of
// "recovered" and "failed" lines every redial interval.
func TestRecoveryNeedsTwoGoodWrites(t *testing.T) {
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	lm := testExporter()
	lm.endpoint = conn.LocalAddr().String()
	// As fail() leaves it: known bad, socket closed, nothing proved since.
	lm.refused = true
	lm.sinceDial = 0

	lm.emit(launchSample{outcome: launchOK, at: time.Now()})
	if !lm.refused {
		t.Error("one write after a redial is not evidence the endpoint is back")
	}
	lm.emit(launchSample{outcome: launchOK, at: time.Now()})
	if lm.refused {
		t.Error("two writes in a row should clear the endpoint-down flag")
	}
}

// An unencodable sample says nothing about the endpoint, and must not be able
// to claim it went away -- nor to make the next good write claim it came back.
func TestEncodeFailureIsNotAnEndpointFailure(t *testing.T) {
	lm := testExporter()
	if lm.encodeFailed || lm.refused {
		t.Fatal("a fresh exporter should have neither flag set")
	}
	// The two flags are distinct fields; setting one must leave the other be.
	lm.encodeFailed = true
	if lm.refused {
		t.Error("an encode failure must not mark the endpoint as refused")
	}
}

func TestReportDropsIsRateLimited(t *testing.T) {
	lm := testExporter()
	lm.endpoint = "127.0.0.1:25888"

	// Nothing dropped: nothing to say, and nothing scheduled.
	lm.reportDrops()
	if !lm.nextDropReport.IsZero() {
		t.Error("a report was scheduled with no drops to report")
	}

	lm.dropped.Store(40)
	lm.reportDrops()
	if lm.reportedDrops != 40 {
		t.Fatalf("reportedDrops = %d, want 40", lm.reportedDrops)
	}
	scheduled := lm.nextDropReport
	if scheduled.IsZero() {
		t.Fatal("the first report did not schedule the next")
	}

	// More drops, immediately: counted, but not spoken about again yet.
	lm.dropped.Store(90)
	lm.reportDrops()
	if lm.reportedDrops != 40 {
		t.Errorf("reportedDrops = %d; a second report inside the interval should be held", lm.reportedDrops)
	}
	if !lm.nextDropReport.Equal(scheduled) {
		t.Error("a held report should not push the schedule out")
	}

	// Once the interval has passed, the backlog is reported in one line.
	lm.nextDropReport = time.Now().Add(-time.Second)
	lm.reportDrops()
	if lm.reportedDrops != 90 {
		t.Errorf("reportedDrops = %d, want 90", lm.reportedDrops)
	}
}

// The regression the review caught: the launch slot must be taken on the
// queue of the conn that is live when the slot is acquired, not the one
// captured when the launch began. An image pull can run for minutes, and a
// worker ejected and recovered in that window has a new conn with a new
// semaphore -- acquiring on the old one exceeds CORK_CONCURRENT_LAUNCHES on
// the live daemon and hides the waiter from admit().
func TestLaunchRecordsAgainstTheLiveQueue(t *testing.T) {
	m := &Manager{}
	original := newDaemonQueue(2)
	m.workers = map[string]*workerConn{"10.0.0.1": {queue: original}}
	m.workerOrder = []string{"10.0.0.1"}

	instance := &InstanceMetadata{Id: 1, Worker: "10.0.0.1"}
	timer := m.beginLaunch(&BuildMetadata{Id: 1, Challenge: "test/chal"}, instance, launchLimits{})

	// The worker is ejected and recovered mid-launch: a new conn, a new queue.
	replacement := newDaemonQueue(2)
	m.workers["10.0.0.1"] = &workerConn{queue: replacement}

	timer.finish(nil)

	if got := replacement.launches.stats().Count; got != 1 {
		t.Errorf("the live queue recorded %d launches, want 1", got)
	}
	if got := original.launches.stats().Count; got != 0 {
		t.Errorf("the retired queue recorded %d launches, want 0", got)
	}
}

// managerWithSink builds a Manager whose exporter drops samples into a
// channel, so a test can read exactly what would have gone to the agent.
func managerWithSink(t *testing.T) (*Manager, *daemonQueue, chan launchSample) {
	t.Helper()
	sink := make(chan launchSample, 8)
	q := newDaemonQueue(2)
	m := &Manager{
		metrics:     &launchMetrics{ch: sink, log: newLogger(DISABLED)},
		workers:     map[string]*workerConn{"10.0.0.1": {queue: q}},
		workerOrder: []string{"10.0.0.1"},
	}
	return m, q, sink
}

func testInstance() *InstanceMetadata {
	return &InstanceMetadata{Id: 1, Worker: "10.0.0.1"}
}

func testBuild() *BuildMetadata {
	return &BuildMetadata{Id: 1, Challenge: "test/chal"}
}

// A rebuild's restart is not a player waiting, and a rebuild of a large
// corpus would push several hundred of them through a 256-sample window. The
// ring is the player-latency view, so restarts stay out of it -- but they are
// still exported, tagged, so nothing is lost.
func TestRestartIsExportedButKeptOutOfTheRing(t *testing.T) {
	m, q, sink := managerWithSink(t)

	m.beginLaunch(testBuild(), testInstance(), m.restartLimits()).finish(nil)

	if got := q.launches.stats().Count; got != 0 {
		t.Errorf("the ring counted %d restarts, want 0", got)
	}
	select {
	case s := <-sink:
		if s.trigger != launchTriggerRestart {
			t.Errorf("trigger = %q, want %q", s.trigger, launchTriggerRestart)
		}
	default:
		t.Error("a restart was not exported")
	}
}

// The counterpart: a launch the platform asked for is what the ring is for.
func TestRequestLaunchEntersTheRing(t *testing.T) {
	m, q, _ := managerWithSink(t)

	m.beginLaunch(testBuild(), testInstance(), launchLimits{}).finish(nil)

	stats := q.launches.stats()
	if stats.Count != 1 || stats.Samples != 1 {
		t.Errorf("stats = %+v, want one counted and one timed", stats)
	}
}

// CloudWatch files a datapoint at the timestamp it carries. A launch stamped
// at its start lands in an evaluation period an alarm may already have closed
// -- and the slowest launches, which matter most, would be latest.
func TestSampleIsStampedAtCompletion(t *testing.T) {
	m, _, sink := managerWithSink(t)

	timer := m.beginLaunch(testBuild(), testInstance(), launchLimits{})
	begun := timer.begun
	time.Sleep(2 * time.Millisecond)
	timer.finish(nil)

	s := <-sink
	if !s.at.After(begun) {
		t.Errorf("at = %s, begun = %s; the record should be stamped when the launch ended", s.at, begun)
	}
	if s.at.Sub(begun) < s.total {
		t.Errorf("at is %s past begun but the launch took %s", s.at.Sub(begun), s.total)
	}
}

func TestEMFCarriesTheTrigger(t *testing.T) {
	event, _ := decodeEvent(t, testExporter(), launchSample{
		outcome: launchOK, worker: "10.0.0.1", trigger: launchTriggerRestart, at: time.Now(),
	})
	if event["Trigger"] != launchTriggerRestart {
		t.Errorf("Trigger = %v, want %q", event["Trigger"], launchTriggerRestart)
	}
}

// A refusal counts, whichever end of the launch it came from. admit() refuses
// before anything reaches a daemon and is the fast path under overload, so a
// refusal recorded only from inside launchStages would count one half of one
// condition -- and go quiet in exactly the conditions worth watching.
func TestRefusalIsCountedAsAFailure(t *testing.T) {
	m, q, sink := managerWithSink(t)

	m.beginLaunch(testBuild(), testInstance(), launchLimits{}).finish(ErrWorkerBusy)

	stats := q.launches.stats()
	if stats.Count != 1 || stats.Failed != 1 {
		t.Errorf("stats = %+v, want one launch counted and one failed", stats)
	}
	if stats.Samples != 0 {
		t.Errorf("Samples = %d, want 0: a refusal is not a launch time", stats.Samples)
	}
	if s := <-sink; s.outcome != launchBusy {
		t.Errorf("outcome = %q, want %q", s.outcome, launchBusy)
	}
}

// The barrier that makes the export unable to take the daemon down with it.
// There is deliberately no way to make emit panic from outside -- that is the
// point -- so the handler is driven directly.
func TestRecoverEmitSwallowsAPanic(t *testing.T) {
	lm := testExporter()

	boom := func() {
		defer lm.recoverEmit()
		panic("a sample from the future")
	}
	boom()

	if got := lm.panics.Load(); got != 1 {
		t.Errorf("panics = %d, want 1", got)
	}
	if !lm.panicked {
		t.Error("a recovered panic should be recorded, so it is reported once rather than per launch")
	}

	// And again: counted, but not said twice.
	boom()
	if got := lm.panics.Load(); got != 2 {
		t.Errorf("panics = %d, want 2", got)
	}
}

// The ordinary path must not be mistaken for a panic.
func TestRecoverEmitIsInertWithoutAPanic(t *testing.T) {
	lm := testExporter()
	func() { defer lm.recoverEmit() }()
	if lm.panics.Load() != 0 || lm.panicked {
		t.Error("recoverEmit fired with nothing to recover")
	}
}

// One line when the endpoint goes, then one every refusedReportInterval for
// as long as it stays gone. Without the repeat, a daemon up for weeks has its
// only evidence scrolled out of reach, and a log read after a missing-data
// alarm cannot tell a broken export from one that recovered hours ago.
//
// Asserted on the schedule rather than on the log output, as reportDrops is:
// the deadline moves only when a line is written.
func TestRefusalIsReportedAgainWhileItPersists(t *testing.T) {
	lm := testExporter()
	lm.endpoint = "127.0.0.1:1"

	lm.fail("writing to %s: %s", errRedialPending)
	if !lm.refused {
		t.Fatal("the first failure should mark the endpoint refused")
	}
	first := lm.nextRefusedReport
	if first.IsZero() {
		t.Fatal("the first failure did not schedule the next report")
	}

	// Still failing, moments later: counted, redialled, but not said again.
	lm.fail("writing to %s: %s", errRedialPending)
	if !lm.nextRefusedReport.Equal(first) {
		t.Error("a failure inside the interval moved the schedule; it should be held")
	}

	// Once the interval has passed, it says so again.
	lm.nextRefusedReport = time.Now().Add(-time.Second)
	lm.fail("writing to %s: %s", errRedialPending)
	if !lm.nextRefusedReport.After(first) {
		t.Error("a failure past the interval did not report again")
	}
}

// Every failure still backs the socket off, whether or not it was spoken
// about: the reporting cadence and the redial cadence are different things,
// and quieting the log must not stop the retry.
func TestAHeldReportStillBacksTheSocketOff(t *testing.T) {
	lm := testExporter()
	lm.endpoint = "127.0.0.1:1"
	lm.fail("writing to %s: %s", errRedialPending)

	lm.nextDial = time.Time{} // as if the backoff had elapsed
	lm.fail("writing to %s: %s", errRedialPending)
	if lm.nextDial.IsZero() {
		t.Error("a failure that was not reported also skipped the redial backoff")
	}
	if lm.conn != nil {
		t.Error("a failure that was not reported left the socket open")
	}
}

// Recovery clears the deadline as well as the flag. Otherwise an endpoint that
// came back and failed again inside five minutes would fail silently -- the
// one case where the operator most needs to be told.
func TestRecoveryClearsTheRefusalSchedule(t *testing.T) {
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	lm := testExporter()
	lm.endpoint = conn.LocalAddr().String()
	lm.refused = true
	lm.nextRefusedReport = time.Now().Add(refusedReportInterval)

	// Two writes in a row, which is what recovery takes.
	lm.emit(launchSample{outcome: launchOK, at: time.Now()})
	lm.emit(launchSample{outcome: launchOK, at: time.Now()})
	if lm.refused {
		t.Fatal("two good writes should clear the endpoint-down flag")
	}
	if !lm.nextRefusedReport.IsZero() {
		t.Error("recovery left a stale report deadline; the next outage would be silent")
	}

	// And a fresh outage is reported at once rather than waiting it out.
	lm.fail("writing to %s: %s", errRedialPending)
	if !lm.refused || lm.nextRefusedReport.IsZero() {
		t.Error("the outage after a recovery was not reported")
	}
}

// The agent takes each event as a line, and its own reference clients
// (aws-embedded-metrics) append a newline to every record on both the UDP and
// TCP endpoints. A datagram sent without one risks being held for a delimiter
// that never comes and discarded, with nothing wrong on this side to find --
// so the framing is asserted on the bytes that actually go out.
func TestDatagramIsNewlineTerminated(t *testing.T) {
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	lm := testExporter()
	lm.endpoint = conn.LocalAddr().String()
	lm.emit(launchSample{outcome: launchOK, worker: "10.0.0.1", total: time.Second, at: time.Now()})

	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 8192)
	n, _, err := conn.ReadFrom(buf)
	if err != nil {
		t.Fatalf("reading the datagram: %s", err)
	}
	raw := string(buf[:n])
	if !strings.HasSuffix(raw, "\n") {
		t.Errorf("the datagram is not newline terminated; the agent may never find its end:\n%q", raw)
	}
	// And exactly one, at the end: a newline inside the object is the thing
	// the format forbids outright.
	if strings.Count(raw, "\n") != 1 {
		t.Errorf("the datagram holds %d newlines, want exactly one at the end", strings.Count(raw, "\n"))
	}
	var event map[string]any
	if err := json.Unmarshal(buf[:n], &event); err != nil {
		t.Fatalf("the terminated datagram no longer parses: %s", err)
	}
}

// A restart pulls before its teardown, so that time is spent before the
// launch it belongs to begins. Carrying it into ImageWait without also
// carrying it into the total published two numbers about one launch that
// could not both be true: a 90s image wait inside a 4s launch.
func TestEnsuredImageWaitIsInsideTheTotal(t *testing.T) {
	m, _, sink := managerWithSink(t)

	limits := launchLimits{imagesEnsured: true, ensuredImageWait: 30 * time.Second, ensuredPulled: true}
	timer := m.beginLaunch(testBuild(), testInstance(), limits)
	timer.images(false) // what launchStages reports: this launch pulled nothing
	timer.finish(nil)

	s := <-sink
	if !s.pulled {
		t.Error("ImagePulled should reflect the caller's pull, not this launch's lack of one")
	}
	if s.imageWait < 30*time.Second {
		t.Errorf("imageWait = %s, want at least the 30s the caller measured", s.imageWait)
	}
	if s.total < s.imageWait {
		t.Errorf("total %s is shorter than its own image stage %s; the stages sum past the launch", s.total, s.imageWait)
	}
}

// daemonQueue falls back to the local queue for a worker it does not know,
// which is right when taking a slot and wrong when counting: worker-list
// never shows the local ring, so a sample put there is counted somewhere
// nobody looks. Better uncounted.
func TestASampleForARemovedWorkerIsNotCountedLocally(t *testing.T) {
	m, _, sink := managerWithSink(t)
	m.localQueue = newDaemonQueue(2)

	instance := testInstance()
	timer := m.beginLaunch(testBuild(), instance, launchLimits{})

	// The worker is removed mid-launch (worker-remove, or a scale-in purge).
	delete(m.workers, instance.Worker)
	m.workerOrder = nil

	timer.finish(nil)

	if got := m.localQueue.launches.stats().Count; got != 0 {
		t.Errorf("the local ring counted %d launches for a removed worker, want 0", got)
	}
	// The record still leaves, naming the worker it actually ran on: the
	// export is not where the ambiguity was.
	s := <-sink
	if s.worker != "10.0.0.1" {
		t.Errorf("exported worker = %q, want the worker it ran on", s.worker)
	}
}

// The local daemon is a real placement on a single-host deployment, and its
// ring is the only one there is.
func TestALocalLaunchIsCountedOnTheLocalRing(t *testing.T) {
	m, _, _ := managerWithSink(t)
	m.localQueue = newDaemonQueue(2)

	m.beginLaunch(testBuild(), &InstanceMetadata{Id: 1, Worker: ""}, launchLimits{}).finish(nil)

	if got := m.localQueue.launches.stats().Count; got != 1 {
		t.Errorf("the local ring counted %d launches, want 1", got)
	}
}

// The dimension is the worker's player-facing name, not the address corkd
// dials. On an autoscaled fleet that name is permanent per EIP slot, so a
// machine replaced from the AMI keeps its series; the private IP would change
// and mint a fresh billable metric for the month. The machine is still
// recorded, as a field, which costs nothing.
func TestTheDimensionIsTheWorkerName(t *testing.T) {
	m, _, sink := managerWithSink(t)
	m.workers["10.0.0.1"].public = "challenge-worker-3"

	m.beginLaunch(testBuild(), testInstance(), launchLimits{}).finish(nil)

	s := <-sink
	if s.worker != "challenge-worker-3" {
		t.Errorf("dimension = %q, want the worker's public name", s.worker)
	}
	if s.workerIP != "10.0.0.1" {
		t.Errorf("workerIP = %q, want the address corkd dials", s.workerIP)
	}

	event, _ := decodeEvent(t, testExporter(), s)
	if event["Worker"] != "challenge-worker-3" {
		t.Errorf("Worker = %v, want the public name", event["Worker"])
	}
	if event["WorkerIP"] != "10.0.0.1" {
		t.Errorf("WorkerIP = %v, want the private address", event["WorkerIP"])
	}
	// The machine must not be a dimension: one metric per machine is what
	// keying on the address would have cost in the first place.
	for _, d := range event["_aws"].(map[string]any)["CloudWatchMetrics"].([]any) {
		for _, set := range d.(map[string]any)["Dimensions"].([]any) {
			for _, key := range set.([]any) {
				if key == "WorkerIP" {
					t.Error("WorkerIP is a dimension; it belongs in the record as a field")
				}
			}
		}
	}
}

// public is optional on worker-add, so a worker registered without one still
// has to be named something an operator recognises.
func TestTheDimensionFallsBackToTheAddress(t *testing.T) {
	m, _, sink := managerWithSink(t)
	m.workers["10.0.0.1"].public = "" // added as `cork worker-add <ip>`

	m.beginLaunch(testBuild(), testInstance(), launchLimits{}).finish(nil)

	s := <-sink
	if s.worker != "10.0.0.1" {
		t.Errorf("dimension = %q, want the address when no public name was given", s.worker)
	}
}

// A slot outlives the machines on it: a replacement keeps the name, so the
// series continues rather than starting again under a new address.
func TestASlotKeepsItsNameAcrossAReplacement(t *testing.T) {
	m, _, sink := managerWithSink(t)
	m.workers["10.0.0.1"].public = "challenge-worker-3"
	m.beginLaunch(testBuild(), testInstance(), launchLimits{}).finish(nil)
	before := <-sink

	// The machine breaks and is replaced from the AMI onto the same slot: new
	// private address, same permanent public name.
	delete(m.workers, "10.0.0.1")
	m.workers["10.0.0.9"] = &workerConn{queue: newDaemonQueue(2), public: "challenge-worker-3"}
	m.workerOrder = []string{"10.0.0.9"}

	m.beginLaunch(testBuild(), &InstanceMetadata{Id: 2, Worker: "10.0.0.9"}, launchLimits{}).finish(nil)
	after := <-sink

	if before.worker != after.worker {
		t.Errorf("the slot's series broke across a replacement: %q then %q", before.worker, after.worker)
	}
	if before.workerIP == after.workerIP {
		t.Fatal("this test proves nothing unless the machine actually changed")
	}
}

// A fleet with nowhere to put a launch refuses before any worker, any row or
// any daemon is involved. That path has to be counted: it is the moment
// LaunchFailed most needs to move, and it was silent.
func TestAnUnplaceableLaunchIsRecorded(t *testing.T) {
	m, q, sink := managerWithSink(t)
	m.localQueue = newDaemonQueue(2)

	m.recordUnplaced(testBuild(), ErrAllWorkersOverloaded)

	s := <-sink
	if s.worker != unplacedWorker {
		t.Errorf("worker = %q, want %q", s.worker, unplacedWorker)
	}
	if s.outcome != launchNoWorker {
		t.Errorf("outcome = %q, want %q", s.outcome, launchNoWorker)
	}
	if s.challenge != "test/chal" {
		t.Errorf("challenge = %q, want the build's", s.challenge)
	}
	// No daemon took it, so no daemon's ring may claim it.
	if got := q.launches.stats().Count; got != 0 {
		t.Errorf("a worker's ring counted %d unplaceable launches, want 0", got)
	}
	if got := m.localQueue.launches.stats().Count; got != 0 {
		t.Errorf("the local ring counted %d unplaceable launches, want 0", got)
	}
}

// Every way the fleet can have nowhere to put a launch reads as the same
// outcome, so one series answers "could we place anything at all".
func TestFleetExhaustionOutcomes(t *testing.T) {
	for _, err := range []error{
		ErrAllWorkersOverloaded, ErrAllWorkersUnresponsive, ErrAllWorkersDown, ErrNoWorkers,
		fmt.Errorf("placing instance 3: %w", ErrAllWorkersDown),
	} {
		if got := launchOutcomeOf(err); got != launchNoWorker {
			t.Errorf("launchOutcomeOf(%v) = %q, want %q", err, got, launchNoWorker)
		}
	}
}

// "unplaced" has to be a usable dimension value: non-empty, and not colliding
// with "local", which is a real daemon that really did take launches.
func TestUnplacedIsAValidDimension(t *testing.T) {
	event, _ := decodeEvent(t, testExporter(), launchSample{
		worker: unplacedWorker, outcome: launchNoWorker, at: time.Now(),
	})
	if event["Worker"] != unplacedWorker {
		t.Errorf("Worker = %v, want %q", event["Worker"], unplacedWorker)
	}
	if event["LaunchFailed"].(float64) != 1 {
		t.Error("an unplaceable launch must count as a failure")
	}
	// A failure publishes no duration, so nothing here can drag the
	// quantiles down.
	for _, name := range metricNames(event) {
		if name != "LaunchFailed" {
			t.Errorf("an unplaceable launch published %q", name)
		}
	}
}

// A nil exporter is every deployment without an agent, and this path runs
// before anything else would have caught it.
func TestRecordUnplacedWithNoExporter(t *testing.T) {
	m := &Manager{}
	m.recordUnplaced(testBuild(), ErrNoWorkers)
}
