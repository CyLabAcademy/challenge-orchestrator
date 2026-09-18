package cork

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/docker/go-connections/tlsconfig"
	"github.com/moby/moby/client"
)

const (
	// WORKER_SERVERNAME is the TLS name every worker's dockerd certificate is
	// issued for (SAN DNS:academy-docker-worker). Workers share one server
	// cert so they can be cloned without reprovisioning; the client pins this
	// name instead of verifying against the dialed IP.
	WORKER_SERVERNAME = "academy-docker-worker"

	workerDockerPort    = 2376
	workerTelemetryPort = 2136

	// minPollInterval floors CORK_WORKER_POLL_INTERVAL. The per-poll timeout
	// is derived from the interval when it does not fit under it, and a
	// timeout of zero would mean no timeout at all (http.Client), so an
	// interval too small to leave room for one is refused rather than
	// silently unbounding the poll.
	minPollInterval = 10 * time.Millisecond
)

// workerTiming holds every worker tunable. defaultWorkerTiming is the
// production setting; each field can be overridden from the environment
// (workerTimingFromEnv), mainly so a test fleet can detect failures faster.
//
//   - pollInterval: probe cadence per worker. Seconds, not milliseconds: the
//     telemetry agent applies its own hysteresis, so there is nothing to see
//     at sub-second resolution, and misses are counted consecutively — at a
//     500ms cadence a worker answering one poll in sixty never trips the
//     threshold at all, where a slower cadence catches it.
//   - pollTimeout: per-telemetry-poll timeout; must stay under pollInterval.
//     Tight on purpose: /health serves a cached verdict, so a slow answer is
//     a sick box rather than a busy one.
//   - pingTimeout: per-ping timeout for the dockerd liveness probe. Looser
//     than pollTimeout — dockerd under load answers /_ping more slowly than
//     the agent answers from an atomic, and there is TLS in the path.
//   - deepProbeInterval: cadence of the deeper dockerd probe, a container
//     list. Ping proves the API goroutine is alive; it does not prove
//     containerd or the container store, and a daemon can answer one while
//     wedging the other. The list is issued with Limit 1, so its cost does
//     not grow with the number of containers the worker holds.
//   - maxMisses: consecutive failed dockerd probes before the worker is
//     ejected as unresponsive. Reversible, unlike the old sticky down, so it
//     can be strict: 6 * 5s = 30s.
//   - loadMisses: consecutive failed telemetry polls before the worker's load
//     is unknown. Tighter than maxMisses because unknown still takes
//     placements — being wrong costs a flag, not a worker.
//   - healthyThreshold: consecutive good probes before an unresponsive worker
//     is recovered. More than one, so a daemon flapping as it starts does not
//     bounce in and out of placement.
//   - recoverBackoff / recoverBackoffMax: wait before a recovery attempt,
//     multiplied by the number of times the worker has been ejected and
//     capped. A box that keeps failing is retried more slowly each time.
//   - ejectionDecay: how long a worker must stay ok to have one ejection
//     forgiven. A decay rather than a reset, so forgiveness is proportional
//     to how badly the box has behaved.
//   - controlTimeout: ceiling for one container/network API call. These
//     normally finish in well under a second; a call that hangs this long
//     means a wedged daemon, so it doubles as an ejection trigger.
//   - pullTimeout: ceiling for one image pull before a request-driven
//     launch. Challenge images are tens of MB over a fast private network,
//     so a pull that takes longer than this is already an incident: it fails
//     the launch as retryable but never ejects the worker, since the
//     registry is the likelier culprit. A restart during a rebuild pulls
//     under its own, longer ceiling (restartLimits in launch.go).
//   - launchWait: how long a launch waits for a launch slot on its daemon
//     before it is refused as busy, a retryable failure. That queue is
//     corkd's own, no daemon involved, so it is kept short: under adverse
//     load the platform's retry lands elsewhere (see launch.go).
type workerTiming struct {
	pollInterval      time.Duration
	pollTimeout       time.Duration
	pingTimeout       time.Duration
	deepProbeInterval time.Duration
	maxMisses         int
	loadMisses        int
	healthyThreshold  int
	recoverBackoff    time.Duration
	recoverBackoffMax time.Duration
	ejectionDecay     time.Duration
	controlTimeout    time.Duration
	pullTimeout       time.Duration
	launchWait        time.Duration
}

var defaultWorkerTiming = workerTiming{
	pollInterval:      5 * time.Second,
	pollTimeout:       250 * time.Millisecond,
	pingTimeout:       time.Second,
	deepProbeInterval: 30 * time.Second,
	maxMisses:         6,
	loadMisses:        3,
	healthyThreshold:  2,
	recoverBackoff:    10 * time.Second,
	recoverBackoffMax: 5 * time.Minute,
	ejectionDecay:     5 * time.Minute,
	controlTimeout:    30 * time.Second,
	pullTimeout:       30 * time.Second,
	launchWait:        10 * time.Second,
}

// workerTimingFromEnv returns defaultWorkerTiming with the CORK_WORKER_*
// overrides applied. A value that does not parse (or is not positive) is
// logged and ignored; the poll interval has a floor (minPollInterval) and a
// poll timeout that does not fit inside it is clamped to half of it, so no
// override can leave a poll unbounded and stall the poller.
func (m *Manager) workerTimingFromEnv() workerTiming {
	t := defaultWorkerTiming
	m.envDuration(WORKER_POLL_INTERVAL_ENV, &t.pollInterval)
	m.envDuration(WORKER_POLL_TIMEOUT_ENV, &t.pollTimeout)
	m.envDuration(WORKER_PING_TIMEOUT_ENV, &t.pingTimeout)
	m.envDuration(WORKER_DEEP_PROBE_ENV, &t.deepProbeInterval)
	m.envDuration(WORKER_RECOVER_BACKOFF_ENV, &t.recoverBackoff)
	m.envDuration(WORKER_RECOVER_BACKOFF_MAX_ENV, &t.recoverBackoffMax)
	m.envDuration(WORKER_EJECTION_DECAY_ENV, &t.ejectionDecay)
	m.envDuration(WORKER_CONTROL_TIMEOUT_ENV, &t.controlTimeout)
	m.envDuration(WORKER_PULL_TIMEOUT_ENV, &t.pullTimeout)
	m.envDuration(WORKER_LAUNCH_WAIT_ENV, &t.launchWait)
	m.envCount(WORKER_MAX_MISSES_ENV, &t.maxMisses)
	m.envCount(WORKER_LOAD_MISSES_ENV, &t.loadMisses)
	m.envCount(WORKER_HEALTHY_THRESHOLD_ENV, &t.healthyThreshold)
	if t.pollInterval < minPollInterval {
		m.log.warnf("worker poll interval %s is below the %s floor; using %s", t.pollInterval, minPollInterval, minPollInterval)
		t.pollInterval = minPollInterval
	}
	// Both per-probe timeouts have to fit inside the interval: a probe still
	// running when the next tick comes round would stall the poller, and a
	// zero timeout means no timeout at all (http.Client).
	m.clampProbeTimeout("poll", &t.pollTimeout, t.pollInterval)
	m.clampProbeTimeout("ping", &t.pingTimeout, t.pollInterval)
	if t.deepProbeInterval < t.pollInterval {
		// The deep probe rides the same tick, so it cannot run more often
		// than one.
		t.deepProbeInterval = t.pollInterval
	}
	if t != defaultWorkerTiming {
		m.log.infof("worker timing: probe every %s (telemetry timeout %s, ping timeout %s, deep probe every %s), "+
			"unresponsive after %d misses, load unknown after %d, recovered after %d good probes, "+
			"recovery backoff %s (max %s), ejection decay %s, control timeout %s, pull timeout %s, launch wait %s",
			t.pollInterval, t.pollTimeout, t.pingTimeout, t.deepProbeInterval,
			t.maxMisses, t.loadMisses, t.healthyThreshold,
			t.recoverBackoff, t.recoverBackoffMax, t.ejectionDecay,
			t.controlTimeout, t.pullTimeout, t.launchWait)
	}
	return t
}

// envCount overrides *n from the named variable when it holds an integer of
// at least 1; anything else is logged and leaves *n alone.
func (m *Manager) envCount(name string, n *int) {
	s, ok := LookupEnv(name)
	if !ok {
		return
	}
	v, err := strconv.Atoi(s)
	if err != nil || v < 1 {
		m.log.warnf("invalid %s value '%s' (want an integer >= 1), keeping %d", name, s, *n)
		return
	}
	*n = v
}

// clampProbeTimeout keeps one probe's timeout under the tick it runs on.
func (m *Manager) clampProbeTimeout(what string, d *time.Duration, interval time.Duration) {
	if *d < interval {
		return
	}
	clamped := interval / 2
	m.log.warnf("worker %s timeout %s does not fit under the probe interval %s; using %s", what, *d, interval, clamped)
	*d = clamped
}

// envDuration overrides *d from the named variable when it holds a positive
// duration; anything else is logged and leaves *d alone.
func (m *Manager) envDuration(name string, d *time.Duration) {
	s, ok := LookupEnv(name)
	if !ok {
		return
	}
	v, err := time.ParseDuration(s)
	if err != nil || v <= 0 {
		m.log.warnf("invalid %s value '%s' (want a positive duration such as 30s), keeping %s", name, s, *d)
		return
	}
	*d = v
}

// timing returns the worker tunables, falling back to the defaults for a
// Manager that was not built by NewManager (tests).
func (m *Manager) timing() workerTiming {
	if m.workerTiming.pollInterval == 0 {
		return defaultWorkerTiming
	}
	return m.workerTiming
}

// Selection failures, distinguished so corkd can map them to 503 vs 500.
var (
	ErrAllWorkersOverloaded   = errors.New("all workers are overloaded")
	ErrAllWorkersUnresponsive = errors.New("no workers are responding")
	ErrAllWorkersDown         = errors.New("every worker has been taken down")
)

// A worker carries two states, not one, because reachability and load are
// different kinds of fact arriving from different agents.
//
// workerReachable answers "can this daemon do anything", from the dockerd
// probes and from transport failures on real control calls. workerLoad
// answers "should we add more here", from the telemetry agent. Fusing them
// into one enum is what made a dead telemetry sidecar able to take a healthy
// box out of the fleet.
//
// Only one value on either axis is asserted rather than observed:
// workerDown, which an operator sets with worker-down and only worker-add
// clears. Everything else is an observation and reverses itself.
type workerReachable int32

const (
	// workerUnresponsive: the daemon is not answering. Observed, and cleared
	// by the probe once it answers again (see recoverWorker).
	workerUnresponsive workerReachable = iota
	// workerDown: asserted by worker-down. Sticky; the probe will not lift
	// it, because the operator knows something the probe cannot.
	workerDown
	// workerReachableOk: probes passing and the worker reconciled.
	workerReachableOk
)

func (r workerReachable) String() string {
	switch r {
	case workerReachableOk:
		return "ok"
	case workerDown:
		return "down"
	default:
		return "unresponsive"
	}
}

type workerLoad int32

const (
	// workerLoadUnknown: telemetry is silent. Still takes placements — a
	// worker whose agent died is very often serving perfectly well, and
	// holding it out costs more than placing on it blind.
	workerLoadUnknown workerLoad = iota
	workerOverloaded             // telemetry reports the box over its high-water mark
	workerLoadOk
)

func (l workerLoad) String() string {
	switch l {
	case workerLoadOk:
		return "ok"
	case workerOverloaded:
		return "overloaded"
	default:
		return "unknown"
	}
}

// workerConn is the runtime state for one worker: a docker client for its
// daemon, the two health axes, and a launch semaphore sized like the local
// one. Health only gates placement of new instances; operations on existing
// instances route through cli regardless.
//
// A conn is a one-shot object. Its two broadcast channels are closed, never
// reopened, and a stale error is recognised by comparing against its cli, so
// a worker that becomes unreachable and then recovers gets a *new* conn
// rather than having its flags flipped back (see recoverWorker).
type workerConn struct {
	ip        string
	public    string // player-facing address (IP or hostname); "" = use ip
	cli       *client.Client
	reachable atomic.Int32 // holds a workerReachable
	load      atomic.Int32 // holds a workerLoad
	// since is when the current reachable state was entered, and reason says
	// what put it there. Both are for operators and for whatever drives
	// remediation: "unresponsive" alone cannot be acted on, "unresponsive for
	// 40s, ping refused" can.
	since  atomic.Int64 // unix nanos
	reason atomic.Pointer[string]
	// ejections counts how many times this worker (at this IP, across conns)
	// has been ejected, decayed one per ejectionDecay of clean running. It
	// stretches the wait before each recovery attempt.
	ejections atomic.Int32
	unreachCh chan struct{} // closed when the worker stops being reachable; waiters give up at once
	// retired is set the moment this conn leaves ok, and is what makes the
	// conn one-shot: unreachCh has been closed by then, and a closed channel
	// stays ready, so the conn can never be trusted for placement again
	// whatever the daemon goes on to do. Recovery builds a new one.
	retired atomic.Bool
	// reconciled is set once the first reconcile pass has finished and the
	// worker is about to be polled. Until then a control call that fails
	// against it says nothing about the box: its daemon is expected to be
	// unreachable while it starts, which is exactly what the pass waits out.
	reconciled atomic.Bool
	// fromRecovery marks a conn that recoverWorker built, as opposed to one
	// from startup or worker-add. It is set inside newWorkerConn, before that
	// function starts the conn's own goroutine, and read-only afterwards --
	// which is what makes a plain field safe here. Setting it on the way back
	// out of newWorkerConn instead is a data race with the poller this conn
	// has by then already been handed to, on precisely the path it exists
	// for: a recovery whose reconcile gives up and ejects. It is what lets an ejection be
	// counted when the conn never reached placement: a failed recovery is a
	// failure to escalate against, where a box that has simply never come up
	// is not.
	fromRecovery bool
	// daemonFailed is set the first time something actually fails against
	// this worker's daemon, and separates the two ways a conn can be
	// unresponsive: not yet proved reachable, or proved unreachable. Every
	// conn starts unresponsive because that is what is true of a box nothing
	// has heard from yet, but "we have not looked" is not grounds for the
	// stop path to skip a docker teardown -- that is how a live container
	// keeps serving with its records deleted. Only ejection is.
	daemonFailed atomic.Bool
	queue        *daemonQueue
	done         chan struct{} // closed on removal/replacement to stop the poller
	// probeNow nudges the poller to probe before its next tick, after a real
	// control call has failed. Buffered and sent to without blocking, so a
	// burst of failing launches costs one extra probe, not one each.
	probeNow chan struct{}
}

type WorkerInfo struct {
	IP     string `json:"ip"`
	Public string `json:"public"`
	// Health is the two axes collapsed into one string, kept for clients
	// written against the single-axis API.
	//
	// Deprecated: read Reachable and Load instead.
	Health    string `json:"health"`
	Reachable string `json:"reachable"`
	Load      string `json:"load"`
	// Since is when the worker entered its current reachable state, and
	// Reason says what put it there ("" while it is ok).
	Since     time.Time `json:"since"`
	Reason    string    `json:"reason,omitempty"`
	Instances int       `json:"instances"`
}

// derivedHealth collapses the two axes onto the old single-axis vocabulary,
// worst first. A client testing for "ok" stays correct; one testing for
// "down" now sees "unresponsive" for a box the probe gave up on, which is
// the distinction the new fields exist to make.
func derivedHealth(r workerReachable, l workerLoad) string {
	if r != workerReachableOk {
		return r.String()
	}
	if l == workerOverloaded {
		return "overloaded"
	}
	return "ok"
}

// EnableWorkerPlacement turns on worker selection for new instances. Only
// corkd calls this; without it, configured workers are still routable for
// operations on their existing instances, but new instances stay local.
func (m *Manager) EnableWorkerPlacement() {
	m.workersMu.Lock()
	m.placementEnabled = true
	m.workersMu.Unlock()
}

// initWorkers loads the persisted worker list and starts a telemetry poller
// per worker. Unreachable workers are marked down by their pollers and skipped
// by placement — never a startup failure.
func (m *Manager) initWorkers() error {
	m.workers = make(map[string]*workerConn)

	var rows []struct {
		Ip     string `db:"ip"`
		Public string `db:"public"`
	}
	if err := m.db.Select(&rows, "SELECT ip, public FROM workers;"); err != nil {
		m.log.errorf("could not load workers: %s", err)
		return err
	}

	for _, row := range rows {
		w, err := m.newWorkerConn(row.Ip, row.Public, false)
		if err != nil {
			return err
		}
		m.workers[row.Ip] = w
		m.workerOrder = append(m.workerOrder, row.Ip)
	}

	if len(rows) > 0 {
		m.log.infof("loaded %d worker(s)", len(rows))
	}
	return nil
}

// workerTLSConfig builds the shared TLS config for worker daemons from the
// standard DOCKER_CERT_PATH material (ca.pem, cert.pem, key.pem), with the
// server name pinned to the shared worker certificate. Returns nil (plain
// tcp) when DOCKER_CERT_PATH is unset.
func workerTLSConfig() (*tls.Config, error) {
	certPath := os.Getenv("DOCKER_CERT_PATH")
	if certPath == "" {
		return nil, nil
	}
	cfg, err := tlsconfig.Client(tlsconfig.Options{
		CAFile:             filepath.Join(certPath, "ca.pem"),
		CertFile:           filepath.Join(certPath, "cert.pem"),
		KeyFile:            filepath.Join(certPath, "key.pem"),
		ExclusiveRootPools: true,
		MinVersion:         tls.VersionTLS12,
	})
	if err != nil {
		return nil, err
	}
	cfg.ServerName = WORKER_SERVERNAME
	return cfg, nil
}

func (m *Manager) newWorkerConn(ip, public string, fromRecovery bool) (*workerConn, error) {
	tlsCfg, err := workerTLSConfig()
	if err != nil {
		m.log.errorf("could not load worker TLS material: %s", err)
		return nil, err
	}

	// The client-wide timeout is a transport-level backstop behind the
	// per-call deadlines (controlCtx and the pull timeouts of launchLimits):
	// the longest of them, so it cuts nothing short.
	//
	// The transport keeps enough idle connections for a burst of launches:
	// their image checks run concurrently, outside the launch slots, and with
	// the default pool of two every later burst would pay an mTLS handshake
	// per call.
	httpClient := &http.Client{
		Transport:     &http.Transport{TLSClientConfig: tlsCfg, MaxIdleConnsPerHost: 32},
		Timeout:       m.transportTimeout(),
		CheckRedirect: client.CheckRedirect,
	}

	// WithHTTPClient must precede WithHost: WithHost configures the transport
	// that is current at the time it runs.
	cli, err := client.NewClientWithOpts(
		client.WithHTTPClient(httpClient),
		client.WithHost(fmt.Sprintf("tcp://%s:%d", ip, workerDockerPort)),
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		m.log.errorf("could not create docker client for worker %s: %s", ip, err)
		return nil, err
	}

	w := &workerConn{
		ip:        ip,
		public:    public,
		cli:       cli,
		queue:     newDaemonQueue(slots(m.launchConcurrency)),
		unreachCh: make(chan struct{}),
		done:      make(chan struct{}),
		probeNow:  make(chan struct{}, 1),
	}
	// Fail closed for placement until the box has proved itself: unresponsive
	// with its load unknown, which is what is actually true of a worker
	// nothing has heard from yet. (This used to fail closed as "overloaded",
	// a fiction that was needed only because the honest state was sticky.)
	// The poller starts once the leftovers of instances cork no longer
	// records on the box are gone (runWorker), so nothing is placed there
	// before their ports and network names are free.
	w.reachable.Store(int32(workerUnresponsive))
	w.load.Store(int32(workerLoadUnknown))
	w.since.Store(time.Now().UnixNano())
	// Before the goroutine below exists, which is the only thing that makes
	// this plain field safe to read from it.
	w.fromRecovery = fromRecovery
	go m.runWorker(w)
	return w, nil
}

// runWorker reconciles the worker (reconcileWorker), then polls it
// (pollWorker). A pass that does not finish is retried at the poll cadence,
// the worker staying out of placement meanwhile, for as long as the poller
// tolerates telemetry silence (maxMisses polls). A daemon that is merely
// still starting when corkd starts or the worker is added therefore costs
// seconds, not a worker-add.
func (m *Manager) runWorker(w *workerConn) {
	t := m.timing()
	switch m.reconcileWithRetries(w, m.reconcileWorker, time.Duration(t.maxMisses)*t.pollInterval, t.pollInterval) {
	case reconcileReady:
		w.reconciled.Store(true)
		// The pass just did real work against the daemon, which is a stronger
		// statement than any probe makes, so the worker joins placement now
		// rather than waiting a tick to be told what it has already proved.
		// Its load stays unknown until telemetry answers, which does not hold
		// it back: unknown places.
		m.setReachable(w, workerReachableOk, "")
		m.pollWorker(w)
	case reconcileEjected:
		// The daemon never answered. Poll anyway: the worker is out of
		// placement, and the probe is what brings it back once the box
		// finishes booting -- where this used to need a worker-add.
		m.pollWorker(w)
	case reconcileAbandoned:
		return
	}
}

// What runWorker does once the reconcile pass is over.
type reconcileOutcome int

const (
	reconcileAbandoned reconcileOutcome = iota // the conn was replaced or removed; do nothing
	reconcileReady                             // the worker may take placements
	reconcileEjected                           // its daemon never answered; probe it back
)

// reconcileWithRetries runs reconcile until it comes back done, waiting
// interval between attempts, for at most budget. It reports whether the
// worker is to be polled.
//
// Past the budget the two ways of not finishing part company. A daemon that
// stayed unreachable ejects the worker, as a hung control call would; the
// poller keeps probing and will recover it once the box answers. A pass that
// reached the daemon but could not finish (a database read failed, or the
// daemon refused a removal) lets the worker join placement anyway, loudly:
// its leftovers hold host ports, so some launches there will fail and be
// retried elsewhere, which is a smaller loss than holding a whole box out of
// the fleet over one container the daemon will not remove.
func (m *Manager) reconcileWithRetries(w *workerConn, reconcile func(*workerConn) reconcileResult, budget, interval time.Duration) reconcileOutcome {
	deadline := time.Now().Add(budget)
	for attempt := 1; ; attempt++ {
		result := reconcile(w)
		if w.gone() {
			return reconcileAbandoned
		}
		if result == reconcileDone {
			return reconcileReady
		}
		if attempt == 1 {
			m.log.warnf("worker %s: reconcile %s; retrying for up to %s before it takes placements", w.ip, result, budget)
		}
		if !time.Now().Before(deadline) {
			if result == reconcileUnreachable {
				m.log.errorf("worker %s: its daemon stayed unreachable for %s; ejecting it until it answers", w.ip, budget)
				m.eject(w, "its daemon did not answer while the worker was being reconciled")
				return reconcileEjected
			}
			m.log.errorf("worker %s: could not be fully reconciled within %s; it takes placements with leftovers that may still hold host ports", w.ip, budget)
			return reconcileReady
		}
		select {
		case <-w.done:
			return reconcileAbandoned
		case <-time.After(interval):
		}
	}
}

// controlCtx bounds one docker control-plane call (container/network
// create/start/inspect/remove). The caller must invoke cancel.
func (m *Manager) controlCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(m.ctx, m.timing().controlTimeout)
}

// transportTimeout is the ceiling the worker's HTTP client puts on any one
// call: the longest of the per-call deadlines, so that it only ever catches
// a call that carries none of its own.
func (m *Manager) transportTimeout() time.Duration {
	return max(m.restartLimits().pullTimeout, m.timing().controlTimeout)
}

// pollWorker keeps both of a worker's states current, from two independent
// agents on two ports, and recovers the worker once it answers again. Runs
// until the conn is removed or replaced.
//
// The two probes are not interchangeable, and the asymmetry is the point.
// Telemetry is a tiny stateless agent serving a cached verdict, so its
// silence is strong evidence about the *box* but says nothing about whether
// a launch would work; it drives load only. dockerd is large and fails in
// ordinary ways, but it is the definition of placeable, so it drives
// reachability. A worker whose telemetry unit died is still a worker.
//
// Down is the one state the poller will not touch, in either direction: an
// operator asserted it, and a probe that succeeds is not evidence against
// what they know. Recovery from down stays worker-add.
func (m *Manager) pollWorker(w *workerConn) {
	timing := m.timing()
	telemetryURL := fmt.Sprintf("http://%s:%d/health", w.ip, workerTelemetryPort)
	httpClient := &http.Client{Timeout: timing.pollTimeout}

	var (
		loadMisses int
		misses     int
		goodProbes int
		lastDeep   time.Time
		lastDecay  = time.Now()
		// deepWedged carries the last deep probe's verdict: the daemon
		// answered /_ping but could not list containers. It is not a miss in
		// its own right -- it is what makes the next tick re-test the store
		// rather than take a ping for an answer, since /_ping is served off
		// the daemon's HTTP mux without touching the store and says nothing
		// about it either way. Every tick with a suspicion therefore ends in
		// a real verdict, and the branch that used to count a bare ping as a
		// miss while this was set is gone with the wait it existed for.
		deepWedged bool
		// missReason is what started the current run of misses, kept so the
		// ejection names the probe that actually failed rather than whichever
		// tick happened to cross the threshold.
		missReason string
		// lastSeen is the reachability this loop last observed, so it can
		// tell an ejection it made from one made under it by a failed
		// control call (noteWorkerTransportError).
		lastSeen = workerReachable(w.reachable.Load())
	)

	// The deep probe runs on a slower clock than the ping, so the first tick
	// after start does both: a daemon that answers /_ping while wedged
	// underneath should not be admitted for a whole deepProbeInterval on the
	// strength of the ping alone.
	probe := func() {
		if workerReachable(w.reachable.Load()) == workerDown {
			return
		}

		// A failed control call can eject the worker from under this loop.
		// Notice it, so that the hysteresis below measures from the ejection
		// rather than from whenever this loop last saw a bad probe.
		if cur := workerReachable(w.reachable.Load()); cur != lastSeen {
			if cur != workerReachableOk {
				goodProbes = 0
			}
			lastSeen = cur
		}

		// Load, from telemetry. Never ejects; at worst the load goes unknown.
		if overloaded, err := pollTelemetry(httpClient, telemetryURL); err != nil {
			loadMisses++
			if loadMisses == timing.loadMisses {
				// Once, on the crossing. setLoad logs that the axis moved, but
				// not why, and `reason` carries the reachable axis only -- so
				// without this an operator reads `load: unknown` with nothing
				// to act on, and the causes want different responses. A
				// refused connection is an agent that is not running; a
				// timeout is one that is wedged, or a box too loaded to answer
				// a request that serves a cached value; a status is an agent
				// that is running and declining to give a verdict, which is a
				// signal about the box rather than about the agent and which
				// restarting does nothing for.
				m.log.warnf("worker %s: telemetry stopped answering, load unknown: %s", w.ip, err)
			}
			if loadMisses >= timing.loadMisses {
				m.setLoad(w, workerLoadUnknown)
			}
			// Below the threshold: keep the last verdict to ride out a blip.
		} else {
			loadMisses = 0
			if overloaded {
				m.setLoad(w, workerOverloaded)
			} else {
				m.setLoad(w, workerLoadOk)
			}
		}

		// Reachability, from dockerd. Ping every tick; the deeper container
		// list on its own cadence, because ping only proves the API goroutine
		// is alive and a daemon can answer it while containerd is wedged.
		err := m.pingWorker(w, timing.pingTimeout)
		// On its own slow cadence when the daemon looks fine, and on every
		// tick once it does not.
		//
		// Re-testing a suspicion at once is what stops a single slow list from
		// being fatal. The latch below is only cleared by a deep probe that
		// succeeds, and the ordinary cadence is no shorter than the whole
		// ejection budget (30s against maxMisses x pollInterval), so waiting
		// it out meant one list that merely took too long ejected the worker
		// with certainty -- six ticks of misses and no opportunity to
		// disprove any of them. A Limit-1 list is cheap enough to repeat, so
		// a transient now costs one miss and a wedged store is confirmed
		// every tick instead of once.
		deep := err == nil && (deepWedged || time.Since(lastDeep) >= timing.deepProbeInterval)
		if deep {
			err = m.deepProbeWorker(w)
			// Stamped after the call, not before. A probe that spends its
			// whole timeout has already used up part of its own interval, and
			// timing the next one from before it means a slow probe schedules
			// the next one sooner -- at the limit, every tick -- which is the
			// opposite of what a slow-cadence probe is for.
			lastDeep = time.Now()
			deepWedged = err != nil
		}

		if err != nil {
			goodProbes = 0
			misses++
			missReason = probeReason(deep, err)
		} else {
			misses = 0
			goodProbes++
		}

		if misses > 0 {
			if misses >= timing.maxMisses {
				m.eject(w, missReason)
				lastSeen = workerReachable(w.reachable.Load())
			}
			return
		}
		if workerReachable(w.reachable.Load()) == workerReachableOk {
			// Steady state. Forgive one ejection per decay period of clean
			// running, so a box that had one bad afternoon is not still
			// being retried slowly a week later, while one that has failed
			// thirty times has to earn it back.
			if n := w.ejections.Load(); n > 0 && time.Since(lastDecay) >= timing.ejectionDecay {
				w.ejections.Add(-1)
				lastDecay = time.Now()
			}
			return
		}

		// Unresponsive and answering again.
		if !m.readyToRecover(w, goodProbes, timing) {
			return
		}
		m.recoverWorker(w)
	}

	probe()
	ticker := time.NewTicker(timing.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-w.done:
			return
		case <-w.probeNow:
			// A real control call just failed. Probe now rather than at the
			// next tick: the ejection it implies should be confirmed, and a
			// recovery should start, without waiting out the interval.
			probe()
		case <-ticker.C:
			probe()
		}
	}
}

// probeReason describes which probe failed, for the operator and for
// whatever drives remediation: a daemon that answers /_ping but cannot list
// containers is a different diagnosis from one that answers nothing.
func probeReason(deep bool, err error) string {
	if deep {
		return "dockerd answered /_ping but could not list containers: " + err.Error()
	}
	return "dockerd did not answer: " + err.Error()
}

// pingWorker is the cheap dockerd liveness probe: GET /_ping, served off the
// daemon's HTTP mux without touching the container store, so it costs
// essentially nothing and can run on every tick.
func (m *Manager) pingWorker(w *workerConn, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(m.ctx, timeout)
	defer cancel()
	_, err := w.cli.Ping(ctx, client.PingOptions{})
	return err
}

// deepProbeWorker proves what the ping cannot: that the daemon can actually
// reach its container store. Limit 1 stops it after the first container, so
// the cost does not grow with how many the worker holds -- which is what
// makes it safe to leave on during an event, where a full listing would
// contend with the creates it is meant to protect.
// It is bounded by pingTimeout, not controlTimeout, because it is a probe and
// not a control call. controlTimeout is sized for a create or a remove and is
// six times the poll interval: a store that hangs rather than erroring would
// park the whole poll goroutine for it, and since the deep probe's own
// interval is no longer than that ceiling, each hung call would leave the next
// tick due for another one. The probe would become the tick, misses would
// advance once per ceiling instead of once per interval, and the 30s ejection
// window this file documents would silently become six times that -- slowest
// for exactly the wedged daemon the probe exists to catch. pingTimeout is
// already clamped under the interval (clampProbeTimeout), so it cannot be
// configured into the same shape.
func (m *Manager) deepProbeWorker(w *workerConn) error {
	ctx, cancel := context.WithTimeout(m.ctx, m.timing().pingTimeout)
	defer cancel()
	_, err := w.cli.ContainerList(ctx, client.ContainerListOptions{All: true, Limit: 1})
	return err
}

// recoverBackoffFor is how long a worker must stay ejected before recovery is
// attempted: the base wait multiplied by how many times it has been ejected,
// capped. A box that keeps failing is retried more and more slowly, instead
// of thrashing in and out of placement and taking launches with it each time.
// readyToRecover reports whether an unresponsive worker that is answering
// again has earned its place back: it has waited out the backoff its ejection
// count buys, and has answered a few probes in a row, so a daemon flapping as
// it starts does not bounce in and out of placement.
//
// The wait is measured from the conn's `since`, which setReachable moves on
// every transition, rather than from a timestamp the poller keeps for itself.
// A worker can be ejected from under its poller by a failed control call —
// the primary ejection path, since a launch discovers a wedged daemon well
// before a probe does — and setReachable will not report that transition to a
// second caller. A poller-local written only on the probe path would
// therefore still hold the conn's admission time, an hour old on a box that
// had been healthy all day, and every control-call ejection would skip the
// backoff entirely for the rest of that conn's life. That is precisely the
// ejection the backoff exists to throttle.
func (m *Manager) readyToRecover(w *workerConn, goodProbes int, timing workerTiming) bool {
	if goodProbes < timing.healthyThreshold {
		return false
	}
	return time.Since(time.Unix(0, w.since.Load())) >= m.recoverBackoffFor(w)
}

func (m *Manager) recoverBackoffFor(w *workerConn) time.Duration {
	t := m.timing()
	n := w.ejections.Load()
	if n < 1 {
		n = 1
	}
	backoff := time.Duration(n) * t.recoverBackoff
	return min(backoff, t.recoverBackoffMax)
}

// setReachable is the only writer of a worker's reachable state. Everything
// that can change it — the probes, a failed control call, worker-down — goes
// through here, so there is exactly one place that decides what a transition
// is allowed to be and exactly one that closes unreachCh.
//
// Two rules, both of which used to be spread across the callers:
//
//   - workerDown is asserted, so no observation may lift it. A probe that
//     succeeds against a box an operator took down leaves it down.
//   - leaving workerReachableOk closes unreachCh exactly once, waking every
//     launch queued on that daemon so it can be retried elsewhere instead of
//     waiting out its full slot wait.
//
// It reports whether it made the transition, so a caller that must act only
// once (counting an ejection, say) can tell whether it was the one that moved
// the worker.
func (m *Manager) setReachable(w *workerConn, to workerReachable, reason string) bool {
	for {
		prev := workerReachable(w.reachable.Load())
		if prev == to {
			return false
		}
		if prev == workerDown && to != workerDown {
			// Asserted, and an observation is not evidence against it.
			return false
		}
		if to == workerReachableOk && w.retired.Load() {
			// This conn has already left ok, so its unreachCh is closed, and
			// a closed channel stays ready forever: putting it back into
			// placement would give every launch that selects on it an instant
			// failure, for as long as the conn lived. Recovery replaces the
			// conn precisely so this never has to work (see recoverWorker),
			// and the poller will do that on its next pass.
			return false
		}
		if !w.reachable.CompareAndSwap(int32(prev), int32(to)) {
			continue
		}
		w.since.Store(time.Now().UnixNano())
		if reason == "" {
			w.reason.Store(nil)
		} else {
			w.reason.Store(&reason)
		}
		if prev == workerReachableOk {
			w.retired.Store(true)
			if w.unreachCh != nil {
				close(w.unreachCh)
			}
		}
		if reason != "" {
			m.log.infof("worker %s: %s -> %s (%s)", w.ip, prev, to, reason)
		} else {
			m.log.infof("worker %s: %s -> %s", w.ip, prev, to)
		}
		return true
	}
}

// setLoad is the only writer of a worker's load state. Unlike reachability it
// has no sticky value and gates nothing but placement, so it is a plain
// store — but it is still routed through one function so that the log of a
// worker's life reads from a single place.
func (m *Manager) setLoad(w *workerConn, to workerLoad) {
	if prev := workerLoad(w.load.Swap(int32(to))); prev != to {
		m.log.infof("worker %s: load %s -> %s", w.ip, prev, to)
	}
}

// eject takes a worker out of placement after the probes or a real control
// call gave up on it, and counts the ejection so the next recovery attempt
// waits longer. Reversible: the poller keeps probing and recovers the worker
// once its daemon answers again.
func (m *Manager) eject(w *workerConn, reason string) {
	// Set before the transition, and outside it: a conn whose own reconcile
	// pass gave up is ejected without changing state, because it started
	// unresponsive. That worker's daemon has still demonstrably failed, and
	// it is the case that most needs the stop path to know.
	first := w.daemonFailed.CompareAndSwap(false, true)

	// Count the ejection, which is what stretches the next recovery wait.
	//
	// A transition is the ordinary case: a worker that was in placement has
	// left it. The second clause covers the one that is not, and without it
	// the escalation is dead precisely where it is needed. A conn built by
	// recoverWorker enters unresponsive, so if its reconcile then gives up,
	// setReachable reports no transition and the count would never move --
	// leaving a box whose daemon answers pings but whose reconcile keeps
	// failing to rebuild a connection, spawn a poller and hammer the daemon
	// for a fresh reconcile budget every backoff, forever, at a fixed period.
	// That is the thrash the backoff exists to damp.
	//
	// A worker that has never been admitted at all is still not counted: its
	// first conn comes from startup or worker-add, not from a recovery, and a
	// box that is merely slow to boot should not begin with a stretched wait.
	if m.setReachable(w, workerUnresponsive, reason) || (first && w.fromRecovery) {
		w.ejections.Add(1)
	}
}

// recoverWorker brings an unresponsive worker back, by replacing its conn
// rather than clearing its flags — which is also exactly what worker-add
// does, and for the same reasons.
//
// A conn cannot be reused across an outage. Its unreachCh is a broadcast that
// has already been closed, and a closed channel stays ready forever, so a
// recovered worker sharing it would fail every launch that selects on it. Its
// cli is what tells a late error from a live one (noteWorkerTransportError):
// a call hung since before the outage can return long afterwards, and
// discarding it depends on the pointer having changed. And its launch queue
// may still hold slots taken by calls that have not finished unwinding.
//
// The replacement enters at unresponsive, like any new conn, and joins
// placement only once its reconcile pass has cleared whatever the outage left
// behind — the containers and networks of instances that were stopped while
// the box was out of reach, which would otherwise hold host ports against new
// launches.
func (m *Manager) recoverWorker(w *workerConn) {
	m.workersMu.Lock()
	defer m.workersMu.Unlock()

	// A worker-add or worker-remove may have replaced or purged this conn
	// while the probe ran; either way it is no longer ours to recover.
	if current, ok := m.workers[w.ip]; !ok || current != w {
		return
	}

	// An operator may have taken the box down while the probe ran, and a
	// probe takes long enough for that to be an ordinary sequence rather
	// than a tight race: the deep probe alone can sit in a ContainerList for
	// the whole control timeout, and worker-down before a reboot is exactly
	// what an operator does to a worker that has been flapping.
	//
	// setReachable refuses to lift an asserted down, but recovery does not go
	// through setReachable — it discards this conn for a fresh one, and a
	// fresh conn carries no assertion. Without this check the assertion is
	// not overruled, it is dropped: worker-list reports down, the operator
	// reboots the box, and cork places player instances on it meanwhile.
	if workerReachable(w.reachable.Load()) == workerDown {
		return
	}

	fresh, err := m.newWorkerConn(w.ip, w.public, true)
	if err != nil {
		// Rebuilding the client failed (TLS material unreadable): leave the
		// worker ejected and try again on the next probe.
		m.log.errorf("worker %s: answering again but its connection could not be rebuilt: %s", w.ip, err)
		return
	}
	// Ejections belong to the box, not the conn: they are what slows repeated
	// recovery attempts on a worker that keeps failing. fromRecovery is set
	// here, before the conn is published, so that an ejection ending this
	// attempt counts even though the conn never reached placement (eject).
	fresh.ejections.Store(w.ejections.Load())

	m.workers[w.ip] = fresh
	close(w.done)
	w.cli.Close()
	m.log.infof("worker %s: answering again after %s; reconnecting and reconciling before it takes placements",
		w.ip, time.Since(time.Unix(0, w.since.Load())).Round(time.Second))
}

// markWorkerDown asserts down, which no probe will lift. Reached only from
// SetWorkerDown; the observed paths eject instead.
func (m *Manager) markWorkerDown(w *workerConn) {
	m.setReachable(w, workerDown, "taken down by an operator")
}

// noteWorkerTransportError ejects a worker after a connection-level docker
// failure on cli, the connection the call went out on, and asks its poller to
// probe at once.
//
// One failure is enough, and deliberately so: a control call that failed is
// evidence already paid for, about the daemon that actually matters, and
// discarding it to wait for a synthetic probe would waste it. That is how
// load balancers do it — eject on real traffic, probe only to cover hosts
// that are not receiving any. What made this a hair trigger before was not
// the single failure but the verdict being terminal; now that the worker
// probes its own way back, being wrong costs a probe interval.
//
// API-level errors pass through untouched (see isTransportError).
func (m *Manager) noteWorkerTransportError(worker string, cli *client.Client, err error) {
	if worker == "" || !isTransportError(err) {
		return
	}
	m.workersMu.RLock()
	w, ok := m.workers[worker]
	m.workersMu.RUnlock()
	if !ok {
		return
	}
	if cli != nil && w.cli != cli {
		// The call went out on a connection that has since been replaced:
		// worker-remove then worker-add, on a box that was rebooted or
		// recreated. A hung call outlives its connection by as much as the
		// transport timeout, and the box answering now has failed at nothing.
		m.log.debugf("worker %s: transport error on a replaced connection, ignoring: %s", worker, err)
		return
	}
	if !w.reconciled.Load() {
		// The worker's first reconcile pass has not finished, and that pass
		// is allowed to find the daemon unreachable for its whole budget: a
		// box that is merely still booting is the case it exists for. Let it
		// reach its own verdict rather than ejecting the worker on its
		// behalf and restarting its backoff.
		m.log.debugf("worker %s: transport error before its first reconcile finished, leaving the verdict to the reconcile: %s", worker, err)
		return
	}
	m.eject(w, "a control call failed: "+err.Error())
	// Confirm it, or recover from it, without waiting out the probe interval.
	select {
	case w.probeNow <- struct{}{}:
	default:
	}
}

// SetWorkerDown marks a worker down administratively, taking it out of
// placement without touching its registry entry or instance records (unlike
// RemoveWorker, which purges both). Intended for a box about to be taken out
// of service, for a reboot, a repair or its termination.
//
// This is the one state the probes will not lift, and that is the whole
// difference between it and the unresponsive a failing probe produces: the
// operator knows something the probe does not, so a box that keeps answering
// after being taken down stays down. Recovery is therefore AddWorker on the
// same IP, which rebuilds the connection and poller, or RemoveWorker and
// AddWorker of the replacement.
//
// Note this also switches that worker's instances to the DB-only stop path
// (see stopInstance): their containers are no longer torn down over docker.
// The worker-add that recovers the box removes what those stops left behind.
// The read and the assertion are one critical section. recoverWorker swaps a
// worker's conn under the write lock, so releasing between them would let it
// land the assertion on a conn that has just been discarded: the PATCH would
// report success and the fresh conn would go on to reconcile and take
// placements. Holding the read lock across both means the two orderings are
// the only ones left, and recoverWorker refuses to recover a worker it finds
// down.
func (m *Manager) SetWorkerDown(ip string) error {
	m.workersMu.RLock()
	defer m.workersMu.RUnlock()
	w, ok := m.workers[ip]
	if !ok {
		return &UnknownIdentifierError{Type: "worker", Name: ip}
	}
	m.markWorkerDown(w)
	return nil
}

// isTransportError reports whether err is a connection-level failure —
// timeout, refused/reset connection, DNS — rather than a docker API error.
// Detection is positive-only: transport failures from the HTTP client surface
// as net.Error/url.Error (or context.DeadlineExceeded from our own per-call
// deadlines), while docker API errors (not found, conflict, bad request, ...)
// are plain typed errors that arrived over a working connection and therefore
// say nothing about the daemon's health.
func isTransportError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	// The docker client reports a refused connection (dockerd not listening
	// while the box is up) with an error type of its own that wraps a
	// message rather than the net.Error, so it has to be asked by name.
	if client.IsErrConnectionFailed(err) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	var urlErr *url.Error
	return errors.As(err, &urlErr)
}

func pollTelemetry(httpClient *http.Client, url string) (bool, error) {
	resp, err := httpClient.Get(url)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("telemetry returned status %d", resp.StatusCode)
	}
	var body struct {
		Overloaded bool `json:"overloaded"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return false, err
	}
	return body.Overloaded, nil
}

// AddWorker registers the worker at the given private IP, with an optional
// player-facing public address (IP or hostname; empty means players are given
// the private IP). Workers are keyed by private IP; re-adding an existing one
// (e.g. to recover a sticky-down worker) tears down its old connection and
// poller, starts fresh, and replaces the public address with the one given.
// Either way the box is reconciled before it takes placements: see
// reconcileWorker.
func (m *Manager) AddWorker(ip, public string) error {
	if _, err := netip.ParseAddr(ip); err != nil {
		return fmt.Errorf("invalid worker IP %q: %w", ip, err)
	}

	m.workersMu.Lock()
	defer m.workersMu.Unlock()

	_, err := m.db.Exec(
		"INSERT INTO workers(ip, public) VALUES (?, ?) ON CONFLICT(ip) DO UPDATE SET public = excluded.public;",
		ip, public,
	)
	if err != nil {
		m.log.errorf("could not persist worker %s: %s", ip, err)
		return err
	}

	w, err := m.newWorkerConn(ip, public, false)
	if err != nil {
		return err
	}

	if old, ok := m.workers[ip]; ok {
		close(old.done)
		old.cli.Close()
		m.workers[ip] = w
		m.log.infof("worker %s: connection rebuilt", ip)
		return nil
	}

	m.workers[ip] = w
	m.workerOrder = append(m.workerOrder, ip)
	m.log.infof("added worker %s", ip)
	return nil
}

// RemoveWorker purges the worker: its instance rows (cascading port and
// container assignments) and its registry entry are deleted, and its poller
// is stopped. Containers still running on a live worker are not touched
// now: the box is expected to be on its way out. Should it ever be re-added,
// reconcileWorker removes them then. This is the path for a box that is
// terminated and recreated rather than rebooted or repaired: remove the old
// entry, worker-add the new box.
func (m *Manager) RemoveWorker(ip string) error {
	m.workersMu.Lock()
	defer m.workersMu.Unlock()

	w, ok := m.workers[ip]
	if !ok {
		return &UnknownIdentifierError{Type: "worker", Name: ip}
	}

	tx, err := m.db.Begin()
	if err != nil {
		m.log.errorf("could not begin worker removal transaction: %s", err)
		return err
	}
	res, err := tx.Exec("DELETE FROM instances WHERE worker = ?;", ip)
	if err == nil {
		_, err = tx.Exec("DELETE FROM workers WHERE ip = ?;", ip)
	}
	if err != nil {
		tx.Rollback()
		m.log.errorf("could not purge worker %s: %s", ip, err)
		return err
	}
	if err := tx.Commit(); err != nil {
		m.log.errorf("could not commit removal of worker %s: %s", ip, err)
		return err
	}

	close(w.done)
	w.cli.Close()
	delete(m.workers, ip)
	for i, o := range m.workerOrder {
		if o == ip {
			m.workerOrder = append(m.workerOrder[:i], m.workerOrder[i+1:]...)
			break
		}
	}

	instances, _ := res.RowsAffected()
	m.log.infof("removed worker %s and purged %d instance record(s)", ip, instances)
	return nil
}

// ListWorkers returns every configured worker with its health and the number
// of instances currently recorded on it.
func (m *Manager) ListWorkers() ([]WorkerInfo, error) {
	counts := []struct {
		Worker string `db:"worker"`
		N      int    `db:"n"`
	}{}
	err := m.db.Select(&counts, "SELECT worker, COUNT(*) AS n FROM instances WHERE worker != '' GROUP BY worker;")
	if err != nil {
		m.log.errorf("could not count instances per worker: %s", err)
		return nil, err
	}
	countMap := make(map[string]int, len(counts))
	for _, c := range counts {
		countMap[c.Worker] = c.N
	}

	m.workersMu.RLock()
	defer m.workersMu.RUnlock()

	infos := make([]WorkerInfo, 0, len(m.workerOrder))
	for _, ip := range m.workerOrder {
		w := m.workers[ip]
		reachable := workerReachable(w.reachable.Load())
		load := workerLoad(w.load.Load())
		reason := ""
		if r := w.reason.Load(); r != nil {
			reason = *r
		}
		infos = append(infos, WorkerInfo{
			IP:        ip,
			Public:    w.public,
			Health:    derivedHealth(reachable, load),
			Reachable: reachable.String(),
			Load:      load.String(),
			Since:     time.Unix(0, w.since.Load()),
			Reason:    reason,
			Instances: countMap[ip],
		})
	}
	return infos, nil
}

// WorkersConfigured reports whether any workers exist; used by corkd to
// decide between the legacy single-host gate and worker placement.
func (m *Manager) WorkersConfigured() bool {
	m.workersMu.RLock()
	defer m.workersMu.RUnlock()
	return len(m.workers) > 0
}

// placeable reports whether a new instance may be placed on the worker: its
// daemon is answering and telemetry is not reporting it over its high-water
// mark. An unknown load places — a worker whose telemetry agent died is very
// often serving perfectly well, and on a small fleet holding it out costs
// more than the risk of adding to a box that turns out to be hot.
func placeable(w *workerConn) bool {
	return workerReachable(w.reachable.Load()) == workerReachableOk &&
		workerLoad(w.load.Load()) != workerOverloaded
}

// selectWorker picks the next worker for a new instance: round robin over the
// configured workers, skipping those that cannot take one. With no workers
// configured it returns "" (place locally). When every worker is skipped the
// error says which kind of exhaustion it was, worst case last: overloaded and
// unresponsive are both states a worker leaves on its own, so they are
// retryable (503), where every worker having been taken down by an operator
// is not going to resolve itself (500).
func (m *Manager) selectWorker() (string, error) {
	m.workersMu.Lock()
	defer m.workersMu.Unlock()

	n := len(m.workerOrder)
	if n == 0 {
		return "", nil
	}

	sawOverloaded, sawUnresponsive := false, false
	for i := 0; i < n; i++ {
		idx := (m.rrCursor + i) % n
		w := m.workers[m.workerOrder[idx]]
		if placeable(w) {
			m.rrCursor = (idx + 1) % n
			return w.ip, nil
		}
		switch workerReachable(w.reachable.Load()) {
		case workerReachableOk:
			sawOverloaded = true // reachable but over its mark
		case workerUnresponsive:
			sawUnresponsive = true
		}
	}

	if sawOverloaded {
		return "", ErrAllWorkersOverloaded
	}
	if sawUnresponsive {
		return "", ErrAllWorkersUnresponsive
	}
	return "", ErrAllWorkersDown
}

// workerPublicAddr resolves the player-facing address for the worker at the
// given private IP: its configured public address, falling back to the
// private IP itself when none is set, and "" for an unknown/purged worker.
func (m *Manager) workerPublicAddr(ip string) string {
	m.workersMu.RLock()
	defer m.workersMu.RUnlock()
	w, ok := m.workers[ip]
	if !ok {
		return ""
	}
	if w.public != "" {
		return w.public
	}
	return ip
}

// workerUnreachable reports whether a docker call to the worker at ip is
// pointless: its daemon is not answering, an operator has taken it down, or
// it has been purged entirely. The two health axes collapse to one question
// here on purpose — every consumer (the stop path, the launch guards, the
// update's restart check) only wants to know whether to bother with the
// daemon, and the distinction between "gave up on it" and "was taken down"
// matters to operators and remediation, not to them.
//
// Note this is narrower than !placeable: a conn that has not yet proved
// itself is not placed on, but it is emphatically still worth calling. Every
// conn starts unresponsive, including the ones corkd builds at startup and
// the one worker-add builds for a live box, and each of them spends its
// reconcile pass in that state with a daemon that is usually answering
// perfectly well. Treating that as unreachable would have the stop path
// delete an instance's records without a teardown, leaving the container
// running under its restart policy and holding a host port cork has just
// handed back to the pool. Only a daemon that has actually failed something
// (daemonFailed, set by eject) counts.
func (m *Manager) workerUnreachable(ip string) bool {
	m.workersMu.RLock()
	defer m.workersMu.RUnlock()
	w, ok := m.workers[ip]
	if !ok {
		return true
	}
	switch workerReachable(w.reachable.Load()) {
	case workerReachableOk:
		return false
	case workerDown:
		return true
	default:
		return w.daemonFailed.Load()
	}
}

// workerUnreachableCh returns a channel closed once the worker stops being
// reachable, for waiting on alongside something else. It is nil, and so never
// ready, for an instance on the local daemon and for a worker that is already
// gone, whose callers check workerUnreachable instead.
//
// The channel belongs to the conn, and a worker that recovers gets a new one
// (recoverWorker), so a closed channel here always means the conn it came
// from is finished with.
func (m *Manager) workerUnreachableCh(worker string) <-chan struct{} {
	if worker == "" {
		return nil
	}
	m.workersMu.RLock()
	defer m.workersMu.RUnlock()
	if w, ok := m.workers[worker]; ok {
		return w.unreachCh
	}
	return nil
}

// instanceClient resolves the docker client for the daemon hosting the
// instance: the local env-configured client for instances with no worker,
// otherwise the worker's client. Instances on purged workers are handled by
// the stop path's short-circuit before this is reached.
func (m *Manager) instanceClient(instance *InstanceMetadata) (*client.Client, error) {
	if instance.Worker == "" {
		if m.externalBuildPlane {
			// Never placed by this daemon (newInstance refuses the launch)
			// and refused at startup (checkExternalBuildPlane); only a
			// record written by someone else since that check gets here.
			err := fmt.Errorf("instance %d was placed on the local daemon, which an external build plane has none of", instance.Id)
			m.log.error(err)
			return nil, err
		}
		return m.cli, nil
	}
	m.workersMu.RLock()
	w, ok := m.workers[instance.Worker]
	m.workersMu.RUnlock()
	if !ok {
		err := fmt.Errorf("instance %d references unknown worker %s", instance.Id, instance.Worker)
		m.log.error(err)
		return nil, err
	}
	return w.cli, nil
}

// daemonQueue holds one daemon's slots: launches (see launch.go) and
// teardowns, as many of each as CORK_CONCURRENT_LAUNCHES. Teardowns have
// slots of their own so that a deluge of stops queues here, bounded, rather
// than inside dockerd, which serializes the network side of each removal:
// left to queue there, a mass stop made the daemon slow, then unresponsive,
// and a launch's network create behind it ran into the control timeout and
// marked the healthy worker down. Two pools rather than one so that stops and
// launches do not starve each other.
//
// The queue also keeps what admission (admit in launch.go) reads: how many
// launches are waiting for a launch slot, and a running estimate of how long
// a launch slot is held.
type daemonQueue struct {
	launchSem   chan struct{}
	teardownSem chan struct{}
	waiting     atomic.Int32 // launches waiting for a launch slot
	holdNanos   atomic.Int64 // recent hold time of a launch slot (recordHold)
}

func newDaemonQueue(slots int) *daemonQueue {
	return &daemonQueue{
		launchSem:   make(chan struct{}, slots),
		teardownSem: make(chan struct{}, slots),
	}
}

// recordHold folds one launch slot's hold time into the running estimate: a
// quarter of the new sample and three quarters of the old, so the estimate
// follows the daemon's current pace within a handful of launches, and a
// daemon coming out of a wedge sheds its inflated estimate as quickly.
// Concurrent updates may lose one another's sample; an estimate can afford
// that.
func (q *daemonQueue) recordHold(d time.Duration) {
	old := time.Duration(q.holdNanos.Load())
	if old == 0 {
		q.holdNanos.Store(int64(d))
		return
	}
	q.holdNanos.Store(int64(old*3/4 + d/4))
}

// expectedWait estimates how long a launch placed on the daemon now would
// wait for a launch slot. A launch waits for nobody while a slot is free, so
// only the launches ahead of it beyond the slots count: each of those has to
// finish first, at the recent hold time, and the slots work through them
// together.
//
// A daemon with room therefore estimates zero whatever its hold time, which
// is what keeps a slow spell from outliving itself: the launches after it go
// through, and their samples bring the estimate down. The estimate is zero as
// well until a hold time has been measured, and for the nil queue of a
// Manager without a daemon (tests).
func (q *daemonQueue) expectedWait() (expected time.Duration, waiting int) {
	if q == nil {
		return 0, 0
	}
	hold := time.Duration(q.holdNanos.Load())
	waiting = int(q.waiting.Load())
	slots := cap(q.launchSem)
	ahead := waiting + len(q.launchSem)
	if hold == 0 || ahead < slots {
		return 0, waiting
	}
	return hold * time.Duration(ahead-slots+1) / time.Duration(slots), waiting
}

// daemonQueue returns the queue of the daemon hosting the instance; each
// worker has its own so throughput scales with the fleet.
func (m *Manager) daemonQueue(instance *InstanceMetadata) *daemonQueue {
	if instance.Worker == "" {
		return m.localQueue
	}
	m.workersMu.RLock()
	defer m.workersMu.RUnlock()
	if w, ok := m.workers[instance.Worker]; ok {
		return w.queue
	}
	return m.localQueue
}

// slots guards a semaphore size against a Manager that skipped initDocker
// (tests): an unbuffered channel would never admit anyone.
func slots(n int) int {
	if n < 1 {
		return 1
	}
	return n
}
