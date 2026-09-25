package cork

import (
	"encoding/json"
	"errors"
	"net"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Launch latency, measured where the stages already are (launchStages in
// launch.go) and reported two ways, neither of which keeps a time series
// here:
//
//   - a bounded ring per daemon, which worker-list summarizes. It is what
//     answers "is this worker slow right now" where no CloudWatch is
//     reachable: an e2e run, the test VM, or an operator on the box during
//     an incident.
//   - one embedded-metric-format line per launch to a local CloudWatch
//     agent, which does the aggregation and the shipping. cork holds each
//     sample only until the emitter goroutine has written it.
//
// Nothing here may slow a launch, so the launch goroutine does only the
// cheap half: a handful of time.Now calls, one ring insert, and one
// non-blocking channel send. The marshal and the UDP write belong to the
// emitter goroutine. A CloudWatch agent that is slow, wedged or not running
// at all therefore cannot reach the launch path -- when the channel is full
// the sample is dropped and counted, because a launch is never delayed to
// record one.

const (
	// EMF_ENDPOINT_ENV is the CloudWatch agent's embedded-metric-format
	// socket, "host:port". Unset disables the export entirely, which is the
	// default: a UDP write to a port nothing is listening on draws an ICMP
	// refusal, and a daemon with no agent beside it (a workstation, the e2e,
	// the test VM) should not be generating those. The deployed orchestrators
	// set it; see the cloudwatch_agent role.
	EMF_ENDPOINT_ENV = "CORK_EMF_ENDPOINT"
	// EMF_LOG_GROUP_ENV is the log group the agent files these under. It is
	// carried in the payload rather than the agent's config because the agent
	// requires it there.
	EMF_LOG_GROUP_ENV = "CORK_EMF_LOG_GROUP"
	// EMF_NAMESPACE_ENV is the CloudWatch namespace of the extracted metrics.
	EMF_NAMESPACE_ENV = "CORK_EMF_NAMESPACE"
)

const (
	defaultEMFLogGroup  = "cork"
	defaultEMFNamespace = "cork"
	// emfQueueDepth is how many samples may wait for the emitter. A launch
	// takes seconds and the emitter takes microseconds, so this is only ever
	// touched by a burst against a stalled write; deep enough to ride one
	// out, shallow enough that a wedged agent cannot grow the heap.
	emfQueueDepth = 1024
	// emfRedialInterval is how long the emitter waits before rebuilding a
	// socket that failed. Without it a refused agent would redial once per
	// launch.
	emfRedialInterval = 30 * time.Second
	// refusedReportInterval is how often an endpoint that is still refusing
	// says so again. One line when it goes is not enough: on a daemon up for
	// weeks it scrolls out of reach, and an operator who comes to the log
	// after a missing-data alarm finds nothing recent to say whether the
	// export is still broken or came back hours ago. Five minutes keeps the
	// evidence fresh during an incident without a long outage burying the
	// rest of the log.
	refusedReportInterval = 5 * time.Minute
)

// launchOutcome labels how a launch ended, for the Outcome field. Not a
// CloudWatch dimension: it would multiply every metric by its cardinality to
// answer a question Logs Insights answers from the field for nothing.
// What asked for a launch. A restart during a rebuild is not a player
// waiting, so the two are told apart rather than averaged together.
const (
	launchTriggerRequest = "request"
	launchTriggerRestart = "restart"
)

const (
	launchOK          = "ok"
	launchBusy        = "busy"
	launchUnreachable = "unreachable"
	launchPullTimeout = "pull-timeout"
	launchNoWorker    = "no-worker"
	launchFailed      = "error"
)

// launchSample is one launch: its stage timings, and the state of the daemon
// it ran against when it was admitted. The context travels with the sample
// because a duration on its own cannot be diagnosed -- a 45s launch behind
// seven queued launches on a busy daemon is a different fact from a 45s
// launch on an idle one, and cork is the only process that knows which it
// was. Carrying it here is what makes each record answer for itself instead
// of having to be correlated against something else.
type launchSample struct {
	// worker is the dimension: the player-facing name when the worker has
	// one, its private IP otherwise. See payload for why that way round.
	worker string
	// workerIP is the private address corkd dials, carried as a field so a
	// record can still be pinned to the machine that served it.
	workerIP   string
	challenge  string
	build      int64
	instance   int64
	outcome    string
	total      time.Duration
	imageWait  time.Duration
	slotWait   time.Duration
	netCreate  time.Duration
	containers time.Duration
	waiting    int // launches queued on the daemon when this one was admitted
	slotsBusy  int // launch slots in use at the same moment
	pulled     bool
	trigger    string
	at         time.Time
}

// launchRing holds the most recent launch durations for one daemon. A ring
// rather than a running average because a p50 and a tail cannot be recovered
// from a mean, and the tail is the number worth looking at; 256 samples is
// 2 KiB per daemon and a few minutes of a busy worker.
//
// Only launches that succeeded go in the ring -- a refusal that took three
// milliseconds is not a launch time -- but every launch the platform asked
// for is counted. Counting only the ones that made it would freeze n on a
// worker refusing everything, which reads as "no launches arriving" when the
// truth is the opposite, and that worker is exactly the one this line exists
// to show.
//
// A rebuild's restarts are the exception, and are left out of both: no player
// is waiting on one, and a rebuild of a large corpus would otherwise push
// several hundred through the window. They are exported instead, under
// Trigger, so nothing about them is lost. A rebuild therefore shows here as
// counts that do not move, which is what it is.
type launchRing struct {
	mu     sync.Mutex
	buf    [launchRingSize]time.Duration
	next   int
	filled int   // durations in the ring, capped at launchRingSize
	total  int64 // every launch this daemon has taken, whatever became of it
	failed int64
}

const launchRingSize = 256

func (r *launchRing) add(d time.Duration, ok bool) {
	r.mu.Lock()
	r.total++
	if ok {
		r.buf[r.next] = d
		r.next = (r.next + 1) % launchRingSize
		if r.filled < launchRingSize {
			r.filled++
		}
	} else {
		r.failed++
	}
	r.mu.Unlock()
}

// LaunchStats summarizes a daemon's recent launches. Count and Failed are
// every launch since the process started; the quantiles are over Samples, the
// successful launches still in the ring. The three counts are reported apart
// because a worker that is fast and a worker that is refusing everything
// otherwise look the same from here.
type LaunchStats struct {
	Count   int64 `json:"count"`
	Failed  int64 `json:"failed"`
	Samples int64 `json:"samples"`
	P50Ms   int64 `json:"p50_ms"`
	P90Ms   int64 `json:"p90_ms"`
	MaxMs   int64 `json:"max_ms"`
}

// stats sorts a copy of the ring. Sorting 256 durations costs microseconds
// and happens only when someone asks (worker-list), never on a launch.
func (r *launchRing) stats() LaunchStats {
	if r == nil {
		return LaunchStats{}
	}
	r.mu.Lock()
	total, failed, n := r.total, r.failed, r.filled
	sorted := make([]time.Duration, n)
	copy(sorted, r.buf[:n])
	r.mu.Unlock()

	stats := LaunchStats{Count: total, Failed: failed, Samples: int64(n)}
	if n == 0 {
		// Every launch failed, or none has happened yet. Either way there is
		// no duration to quantile, and the counts above say which.
		return stats
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	stats.P50Ms = sorted[quantileIndex(n, 50)].Milliseconds()
	stats.P90Ms = sorted[quantileIndex(n, 90)].Milliseconds()
	stats.MaxMs = sorted[n-1].Milliseconds()
	return stats
}

// quantileIndex is the nearest-rank index into n sorted samples: the lowest
// value at or above the percentile, which for a handful of samples is the
// honest answer where an interpolated one would invent a number never
// measured.
func quantileIndex(n, pct int) int {
	i := (n*pct + 99) / 100 // ceil(n*pct/100)
	if i < 1 {
		i = 1
	}
	if i > n {
		i = n
	}
	return i - 1
}

// launchMetrics owns the export. A nil *launchMetrics is a working no-op, so
// a Manager that never built one (the CLI, tests) costs a nil check per
// launch and nothing else.
type launchMetrics struct {
	// ch is never closed. record() sends on it from the launch goroutine,
	// and a send on a closed channel panics -- on the one path that must
	// never panic. The exporter therefore has no shutdown: it lives as long
	// as the process, which for a daemon is the whole point.
	ch      chan launchSample
	dropped atomic.Int64
	sent    atomic.Int64
	panics  atomic.Int64

	endpoint  string
	logGroup  string
	namespace string
	log       *logger

	// Everything below belongs to the emitter goroutine alone; no lock.
	conn     net.Conn
	nextDial time.Time
	// refused records that the endpoint is not taking writes, so a missing
	// agent is reported once on the way down and once on the way back rather
	// than once per launch.
	refused bool
	// sinceDial counts writes that returned no error since the socket was
	// built. A connected UDP socket surfaces a refusal on the write *after*
	// the one that drew the ICMP, so the first write following a dial
	// succeeds even against a dead endpoint; only the second proves the
	// endpoint is really there. Without this a redial every 30s against an
	// agent that is down produced an endless "recovered"/"failed" pair --
	// precisely the per-launch noise refused exists to prevent.
	sinceDial int
	// encodeFailed is kept apart from refused: a sample that will not
	// marshal says nothing about the endpoint, and folding the two together
	// made a marshal failure claim the endpoint had gone and the next good
	// write claim it had come back.
	encodeFailed bool
	// panicked records that the barrier in guardedEmit has fired, so a
	// pathological sample arriving on every launch is reported once.
	panicked bool
	// reportedDrops is how many drops have been reported, and nextDropReport
	// when the next report is due (see reportDrops).
	reportedDrops  int64
	nextDropReport time.Time
	// nextRefusedReport is when a still-refusing endpoint may be complained
	// about again. Cleared on recovery, so an outage that follows soon after
	// one is reported at once rather than waiting out a stale deadline.
	nextRefusedReport time.Time
}

// newLaunchMetrics builds the exporter from the environment, or returns nil
// when CORK_EMF_ENDPOINT is unset -- the ring still records either way, so a
// daemon with no agent keeps its local view.
func newLaunchMetrics(log *logger) *launchMetrics {
	endpoint, ok := LookupEnv(EMF_ENDPOINT_ENV)
	if !ok || endpoint == "" {
		return nil
	}
	lm := &launchMetrics{
		ch:        make(chan launchSample, emfQueueDepth),
		endpoint:  endpoint,
		logGroup:  valueOr(Getenv(EMF_LOG_GROUP_ENV), defaultEMFLogGroup),
		namespace: valueOr(Getenv(EMF_NAMESPACE_ENV), defaultEMFNamespace),
		log:       log,
	}
	log.infof("launch metrics: emitting embedded metric format to %s (log group %s, namespace %s)",
		lm.endpoint, lm.logGroup, lm.namespace)
	go lm.run()
	return lm
}

func valueOr(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// record hands one sample to the emitter without ever blocking. A full
// channel means the emitter is behind -- a stalled socket, or a burst larger
// than the queue -- and the sample is dropped rather than made a launch's
// problem. Drops are counted so the loss is visible instead of silent.
func (lm *launchMetrics) record(s launchSample) {
	if lm == nil {
		return
	}
	select {
	case lm.ch <- s:
	default:
		lm.dropped.Add(1)
	}
}

func (lm *launchMetrics) run() {
	for s := range lm.ch {
		// Before the emit rather than after: a write that is taking its time
		// -- a dial waiting on DNS, say -- is exactly what fills the queue,
		// and reporting afterwards would hold the warning for as long as its
		// cause lasted.
		lm.reportDrops()
		lm.guardedEmit(s)
	}
}

// guardedEmit is emit with a panic barrier. A panic on this goroutine would
// take the whole daemon down, and this one is telemetry about launches rather
// than part of one: cork must not stop orchestrating because it could not
// describe what it was doing. Nothing under emit can panic today -- the
// payload is fixed scalar fields and the write is a datagram -- so this
// guards against what a later edit might introduce rather than a known path.
//
// The loop continues afterwards, so one unencodable sample cannot end the
// export; if every sample panics, the exporter simply achieves nothing, at
// the rate launches arrive, and says so once.
func (lm *launchMetrics) guardedEmit(s launchSample) {
	defer lm.recoverEmit()
	lm.emit(s)
}

// recoverEmit turns a panic into a counted, once-reported failure. Split out
// of guardedEmit so a test can drive it: there is deliberately no way to make
// emit panic from outside, which is the point, and a barrier nothing exercises
// is a barrier nobody knows works.
func (lm *launchMetrics) recoverEmit() {
	r := recover()
	if r == nil {
		return
	}
	lm.panics.Add(1)
	if !lm.panicked {
		lm.log.errorf("launch metrics: the exporter panicked (%v); launch latency may stop being exported, launches are unaffected", r)
		lm.panicked = true
	}
}

// dropReportInterval bounds how often a full queue is complained about.
const dropReportInterval = time.Minute

// reportDrops says how many samples the queue turned away since it last said
// so, at most once a dropReportInterval. The counting happens in record, on
// the launch goroutine, which must not log; the saying happens here.
//
// Driven by arriving samples rather than a ticker, so a burst that fills the
// queue and then stops entirely leaves its last drops unreported until the
// next launch. That is the quiet case, and a goroutine and a timer to close
// it would cost more than it is worth.
func (lm *launchMetrics) reportDrops() {
	dropped := lm.dropped.Load()
	if dropped == lm.reportedDrops || time.Now().Before(lm.nextDropReport) {
		return
	}
	lm.log.warnf("launch metrics: dropped %d samples (%d since start) with the queue to %s full; the export is behind, the launches are not",
		dropped-lm.reportedDrops, dropped, lm.endpoint)
	lm.reportedDrops = dropped
	lm.nextDropReport = time.Now().Add(dropReportInterval)
}

// emit marshals one sample and writes it. Every failure is swallowed past
// the first: the export is best-effort by construction, and a daemon whose
// agent is down has better things to say than one line per launch about it.
func (lm *launchMetrics) emit(s launchSample) {
	payload, err := json.Marshal(lm.payload(s))
	if err != nil {
		// Unreachable for these field types; if it ever happens, saying so
		// once beats a silent hole in the metrics.
		if !lm.encodeFailed {
			lm.log.errorf("launch metrics: could not encode a sample: %s", err)
			lm.encodeFailed = true
		}
		return
	}
	conn, err := lm.socket()
	if err != nil {
		return
	}
	// One datagram, one log event, newline-terminated. The agent takes each
	// event as a line: its own reference clients (aws-embedded-metrics) append
	// a newline to every event on both the UDP and TCP endpoints, and the
	// documented shell example pipes its record through echo, which does the
	// same. Sending without one risks the agent holding the record for a
	// delimiter that never arrives -- every launch discarded, silently, with
	// nothing wrong on this side to find. json.Marshal writes no newline of
	// its own, and puts none inside the object either, which is the half of
	// the rule that does hold.
	if _, err := conn.Write(append(payload, '\n')); err != nil {
		lm.fail("writing to %s: %s", err)
		return
	}
	lm.sinceDial++
	// Two, not one: see sinceDial.
	if lm.refused && lm.sinceDial >= 2 {
		lm.log.infof("launch metrics: %s is accepting samples again", lm.endpoint)
		lm.refused = false
		lm.nextRefusedReport = time.Time{}
	}
	lm.sent.Add(1)
}

// socket returns the emitter's connected UDP socket, dialing when there is
// none. A refused or otherwise broken socket is not retried more than once
// per emfRedialInterval: a connected UDP socket reports the ICMP refusal of
// the *previous* write, so an agent that is not running would otherwise have
// cork dialing on every launch.
func (lm *launchMetrics) socket() (net.Conn, error) {
	if lm.conn != nil {
		return lm.conn, nil
	}
	if time.Now().Before(lm.nextDial) {
		return nil, errRedialPending
	}
	conn, err := net.Dial("udp", lm.endpoint)
	if err != nil {
		lm.fail("dialing %s: %s", err)
		return nil, err
	}
	lm.conn = conn
	lm.sinceDial = 0
	return conn, nil
}

// fail closes the socket, schedules a redial and reports the trouble once.
func (lm *launchMetrics) fail(format string, err error) {
	if lm.conn != nil {
		lm.conn.Close()
		lm.conn = nil
	}
	now := time.Now()
	lm.nextDial = now.Add(emfRedialInterval)
	lm.sinceDial = 0
	if lm.refused && now.Before(lm.nextRefusedReport) {
		return
	}
	// Deliberately not the word "dropped": that is what a full queue does to a
	// sample, and is counted and reported separately. An operator reading both
	// lines during a recovery should not have to work out which loss they are
	// looking at.
	lm.log.warnf("launch metrics: "+format+"; launch latency is not being exported until it answers again, launches are unaffected", lm.endpoint, err)
	lm.refused = true
	lm.nextRefusedReport = now.Add(refusedReportInterval)
}

// errRedialPending is returned while the emitter is waiting out a backoff.
// Nothing inspects it -- emit only needs to know not to write -- so it exists
// to keep socket from reporting success with a nil connection.
var errRedialPending = errors.New("waiting to redial the metrics endpoint")

// The embedded metric format, as the agent's socket wants it: one JSON object
// per line, carrying its own log group. CloudWatch extracts the members named
// under Metrics and keeps the whole object as a log event, so anything not
// named there costs nothing beyond the bytes and is still queryable in Logs
// Insights.
//
// What is a dimension and what is a plain field is the whole cost of this
// export: every dimension value mints its own CloudWatch metric, at a monthly
// charge each, while a field is free. So only the two an operator would alarm
// on per worker carry the Worker dimension; the stage breakdown is published
// fleet-wide and cut by worker in Logs Insights instead, where the same
// question costs nothing. Outcome, Challenge and the daemon's state stay
// fields for the same reason -- as dimensions they would multiply every
// metric above by their cardinality.
type emfEvent struct {
	AWS emfMetadata `json:"_aws"`

	// Dimension (string, always present).
	Worker string `json:"Worker"`

	// WorkerIP is the machine behind that name, not a dimension: it changes
	// when a slot's instance is replaced, and paying for a metric per machine
	// to answer what a field answers for nothing is the trade this whole
	// export is built around.
	WorkerIP string `json:"WorkerIP,omitempty"`

	// Metric targets. Always present, so that an event never names a metric
	// whose member is missing; the directives below decide which are
	// published, and a launch that failed publishes no duration at all
	// rather than dragging the quantiles down with its early exit.
	LaunchFailed   int   `json:"LaunchFailed"`
	LaunchDuration int64 `json:"LaunchDuration"`
	ImageWait      int64 `json:"ImageWait"`
	SlotWait       int64 `json:"SlotWait"`
	NetworkCreate  int64 `json:"NetworkCreate"`
	ContainerStart int64 `json:"ContainerStart"`

	// Context. Not metrics and not dimensions: this is what makes one record
	// answer for itself in Logs Insights without being correlated against
	// anything.
	Outcome     string `json:"Outcome"`
	Challenge   string `json:"Challenge,omitempty"`
	Build       int64  `json:"Build,omitempty"`
	Instance    int64  `json:"Instance,omitempty"`
	Waiting     int    `json:"Waiting"`
	SlotsBusy   int    `json:"SlotsBusy"`
	ImagePulled bool   `json:"ImagePulled"`
	Trigger     string `json:"Trigger,omitempty"`
}

type emfMetadata struct {
	Timestamp         int64                `json:"Timestamp"`
	LogGroupName      string               `json:"LogGroupName"`
	CloudWatchMetrics []emfMetricDirective `json:"CloudWatchMetrics"`
}

type emfMetricDirective struct {
	Namespace  string                `json:"Namespace"`
	Dimensions [][]string            `json:"Dimensions"`
	Metrics    []emfMetricDefinition `json:"Metrics"`
}

type emfMetricDefinition struct {
	Name string `json:"Name"`
	Unit string `json:"Unit"`
}

// perWorkerDimensions publishes a metric twice: once cut by worker, once for
// the fleet. The empty set is what gives the fleet-wide series; without it
// every graph would have to sum the workers by hand, and a worker that comes
// and goes with the autoscaling group would leave gaps in it.
var perWorkerDimensions = [][]string{{"Worker"}, {}}

// fleetDimensions publishes one series for the whole orchestrator.
var fleetDimensions = [][]string{{}}

func (lm *launchMetrics) payload(s launchSample) emfEvent {
	worker := s.worker
	if worker == "" {
		// A dimension value may not be empty, and "the daemon on the
		// orchestrator itself" is a real placement -- single-host
		// deployments have no other.
		worker = "local"
	}

	headline := []emfMetricDefinition{{Name: "LaunchFailed", Unit: "Count"}}
	var stages []emfMetricDefinition
	if s.outcome == launchOK {
		headline = append(headline, emfMetricDefinition{Name: "LaunchDuration", Unit: "Milliseconds"})
		stages = []emfMetricDefinition{
			{Name: "ImageWait", Unit: "Milliseconds"},
			{Name: "SlotWait", Unit: "Milliseconds"},
			{Name: "NetworkCreate", Unit: "Milliseconds"},
			{Name: "ContainerStart", Unit: "Milliseconds"},
		}
	}

	directives := []emfMetricDirective{{
		Namespace:  lm.namespace,
		Dimensions: perWorkerDimensions,
		Metrics:    headline,
	}}
	if stages != nil {
		directives = append(directives, emfMetricDirective{
			Namespace:  lm.namespace,
			Dimensions: fleetDimensions,
			Metrics:    stages,
		})
	}

	failed := 1
	if s.outcome == launchOK {
		failed = 0
	}

	return emfEvent{
		AWS: emfMetadata{
			Timestamp:         s.at.UnixMilli(),
			LogGroupName:      lm.logGroup,
			CloudWatchMetrics: directives,
		},
		Worker:         worker,
		WorkerIP:       s.workerIP,
		LaunchFailed:   failed,
		LaunchDuration: s.total.Milliseconds(),
		ImageWait:      s.imageWait.Milliseconds(),
		SlotWait:       s.slotWait.Milliseconds(),
		NetworkCreate:  s.netCreate.Milliseconds(),
		ContainerStart: s.containers.Milliseconds(),
		Outcome:        s.outcome,
		Challenge:      s.challenge,
		Build:          s.build,
		Instance:       s.instance,
		Waiting:        s.waiting,
		SlotsBusy:      s.slotsBusy,
		ImagePulled:    s.pulled,
		Trigger:        s.trigger,
	}
}

// launchTimer times one launch's stages. Inside launch() it is finished from
// a defer, so every exit path through the stages records; admission (admit in
// api.go) refuses before that and finishes one of its own. Both, because how
// often launches are turned away, and how fast, is the same question as how
// long the ones that go through take -- and admission is the fast path under
// overload, so counting only the other would go quiet exactly when it
// mattered.
//
// The whole cost on the launch path is five time.Now calls (a vDSO read
// each), one ring insert and one non-blocking channel send, against a launch
// measured in seconds.
type launchTimer struct {
	m *Manager
	// instance rather than a *daemonQueue: the queue is looked up again when
	// the launch ends, because the one this launch began against may have
	// been retired meanwhile (see finish).
	instance *InstanceMetadata
	limits   launchLimits
	begun    time.Time
	mark     time.Time
	sample   launchSample
}

// beginLaunch starts the clock and snapshots what the daemon was doing when
// this launch was admitted. The snapshot has to be taken here, before the
// launch joins the queue itself, or every sample would report at least one
// launch waiting: its own.
func (m *Manager) beginLaunch(build *BuildMetadata, instance *InstanceMetadata, limits launchLimits) *launchTimer {
	q, public := m.workerDaemon(instance)
	now := time.Now()
	// begun is backed up over any image work the caller did for this launch:
	// restartInstance pulls before its teardown, so that time is already
	// spent by the time we get here. Without it the stages sum past the
	// total, and a cold restart publishes a 90s ImageWait against a 4s
	// LaunchDuration -- two numbers about the same launch that cannot both
	// be true. mark stays at now, since the stage timings below are this
	// launch's own and images() adds the caller's separately.
	t := &launchTimer{m: m, instance: instance, limits: limits,
		begun: now.Add(-limits.ensuredImageWait), mark: now}
	// The public name in preference to the private IP: on an autoscaled fleet
	// it is permanent per EIP slot, so a machine replaced from the AMI takes
	// over the same name and its series continues, where the private IP would
	// change and mint a fresh billable metric for that month. It also reads as
	// a name on a graph rather than as an address. The IP is the fallback,
	// since public is optional, and rides along as a field either way.
	t.sample.worker = public
	if t.sample.worker == "" {
		t.sample.worker = instance.Worker
	}
	t.sample.workerIP = instance.Worker
	t.sample.trigger = launchTriggerRequest
	if limits.restart {
		t.sample.trigger = launchTriggerRestart
	}
	t.sample.instance = int64(instance.Id)
	if build != nil {
		t.sample.challenge = string(build.Challenge)
		t.sample.build = int64(build.Id)
	}
	if q != nil {
		_, waiting := q.expectedWait()
		t.sample.waiting = waiting
		t.sample.slotsBusy = len(q.launchSem)
	}
	return t
}

// split closes the stage that has been running and opens the next.
func (t *launchTimer) split() time.Duration {
	now := time.Now()
	d := now.Sub(t.mark)
	t.mark = now
	return d
}

// images closes the image stage. A caller that ensured the images itself
// spent that time before this launch began, so its measurement is taken from
// the limits rather than from the clock here, which would read zero.
func (t *launchTimer) images(pulled bool) {
	t.sample.imageWait = t.split()
	t.sample.pulled = pulled
	if t.limits.imagesEnsured {
		t.sample.imageWait += t.limits.ensuredImageWait
		t.sample.pulled = t.limits.ensuredPulled
	}
}
func (t *launchTimer) slot()       { t.sample.slotWait = t.split() }
func (t *launchTimer) network()    { t.sample.netCreate = t.split() }
func (t *launchTimer) containers() { t.sample.containers = t.split() }

// finish records the launch against the daemon's ring and hands it to the
// exporter. Only a launch that succeeded contributes a duration, but every
// launch is counted (see launchRing).
//
// The queue is resolved here rather than reused from beginLaunch because a
// launch can outlive the conn it started against: an image pull may run for
// minutes, and a worker ejected and recovered in that window has a new conn
// and a new queue by now. The sample belongs to the daemon as it is, not as
// it was.
func (t *launchTimer) finish(err error) {
	// Stamped at the end, not the start: CloudWatch files a datapoint at the
	// timestamp it carries, and a launch that took four minutes would
	// otherwise land four minutes in the past -- after an alarm had already
	// evaluated that period and moved on. The slowest launches are precisely
	// the ones that must not arrive too late to be seen.
	end := time.Now()
	t.sample.total = end.Sub(t.begun)
	t.sample.at = end
	t.sample.outcome = launchOutcomeOf(err)
	if r := t.ring(); r != nil && !t.limits.restart {
		r.add(t.sample.total, t.sample.outcome == launchOK)
	}
	t.m.metrics.record(t.sample)
}

// ring is the ring this sample belongs in, or nil when there is none to put
// it in. Deliberately not daemonQueue: that falls back to the local queue for
// a worker it does not know, which is right when taking a slot and wrong when
// counting -- a worker removed mid-launch would have its sample counted
// against a ring worker-list never shows. Uncounted beats counted somewhere
// nobody looks.
// It is resolved here, and again in launchStages for the slot, rather than
// once in beginLaunch: ensureImages sits between them and can pull for
// minutes, and a worker ejected and recovered in that window has a new conn
// with a new queue. Taking the slot on the one captured at the start would
// exceed CORK_CONCURRENT_LAUNCHES on the live daemon and hide the waiter from
// admit(). The repeated RLock is the price of that, and it is the cheap half.
func (t *launchTimer) ring() *launchRing {
	if t.instance.Worker == "" {
		if t.m.localQueue == nil {
			return nil
		}
		return &t.m.localQueue.launches
	}
	t.m.workersMu.RLock()
	defer t.m.workersMu.RUnlock()
	if w, ok := t.m.workers[t.instance.Worker]; ok && w.queue != nil {
		return &w.queue.launches
	}
	return nil
}

// unplacedWorker is the dimension for a launch that never reached a worker
// because the fleet had none to give it. Not "local", which is a real daemon,
// and not empty, which is not a dimension value at all.
const unplacedWorker = "unplaced"

// recordUnplaced reports a launch the fleet had nowhere to put. It carries no
// worker and touches no ring -- there is no daemon to attribute it to -- but
// it must still be counted, because a launch nobody could place is a launch
// that failed.
func (m *Manager) recordUnplaced(build *BuildMetadata, err error) {
	if m.metrics == nil {
		return
	}
	s := launchSample{
		worker:  unplacedWorker,
		outcome: launchOutcomeOf(err),
		trigger: launchTriggerRequest,
		at:      time.Now(),
	}
	if build != nil {
		s.challenge = string(build.Challenge)
		s.build = int64(build.Id)
	}
	m.metrics.record(s)
}

// launchOutcomeOf names why a launch ended, using the same distinctions the
// platform retries on.
func launchOutcomeOf(err error) string {
	switch {
	case err == nil:
		return launchOK
	case errors.Is(err, ErrAllWorkersOverloaded), errors.Is(err, ErrAllWorkersUnresponsive),
		errors.Is(err, ErrAllWorkersDown), errors.Is(err, ErrNoWorkers):
		return launchNoWorker
	case errors.Is(err, ErrWorkerBusy):
		return launchBusy
	case errors.Is(err, ErrWorkerUnreachable):
		return launchUnreachable
	case errors.Is(err, ErrPullTimeout):
		return launchPullTimeout
	}
	return launchFailed
}
