package cmgr

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
)

// workerTiming holds every worker tunable. defaultWorkerTiming is the
// production setting; each field can be overridden from the environment
// (workerTimingFromEnv), mainly so a test fleet can detect failures faster.
//
//   - pollInterval: telemetry poll cadence per worker.
//   - pollTimeout: per-poll timeout; must stay under pollInterval.
//   - maxMisses: consecutive failed polls before the worker is marked down
//     (sticky): 60 * 500ms = 30s of telemetry silence.
//   - controlTimeout: ceiling for one container/network API call. These
//     normally finish in well under a second; a call that hangs this long
//     means a wedged daemon, so it doubles as the docker-side down trigger.
//   - pullTimeout: ceiling for one image pull before a request-driven
//     launch. Challenge images are tens of MB over a fast private network,
//     so a pull that takes longer than this is already an incident: it fails
//     the launch as retryable but never marks the worker down, since the
//     registry is the likelier culprit. A restart during a rebuild pulls
//     under its own, longer ceiling (restartLimits in launch.go).
//   - launchWait: how long a launch waits for a launch slot on its daemon
//     before it is refused as busy, a retryable failure. That queue is
//     cmgrd's own, no daemon involved, so it is kept short: under adverse
//     load the platform's retry lands elsewhere (see launch.go).
type workerTiming struct {
	pollInterval   time.Duration
	pollTimeout    time.Duration
	maxMisses      int
	controlTimeout time.Duration
	pullTimeout    time.Duration
	launchWait     time.Duration
}

var defaultWorkerTiming = workerTiming{
	pollInterval:   500 * time.Millisecond,
	pollTimeout:    250 * time.Millisecond,
	maxMisses:      60,
	controlTimeout: 30 * time.Second,
	pullTimeout:    30 * time.Second,
	launchWait:     10 * time.Second,
}

// workerTimingFromEnv returns defaultWorkerTiming with the CMGR_WORKER_*
// overrides applied. A value that does not parse (or is not positive) is
// logged and ignored; a poll timeout that does not fit inside the poll
// interval is clamped to half of it, so no override can stall the poller.
func (m *Manager) workerTimingFromEnv() workerTiming {
	t := defaultWorkerTiming
	m.envDuration(WORKER_POLL_INTERVAL_ENV, &t.pollInterval)
	m.envDuration(WORKER_POLL_TIMEOUT_ENV, &t.pollTimeout)
	m.envDuration(WORKER_CONTROL_TIMEOUT_ENV, &t.controlTimeout)
	m.envDuration(WORKER_PULL_TIMEOUT_ENV, &t.pullTimeout)
	m.envDuration(WORKER_LAUNCH_WAIT_ENV, &t.launchWait)
	if s, ok := os.LookupEnv(WORKER_MAX_MISSES_ENV); ok {
		if n, err := strconv.Atoi(s); err == nil && n >= 1 {
			t.maxMisses = n
		} else {
			m.log.warnf("invalid %s value '%s' (want an integer >= 1), keeping %d", WORKER_MAX_MISSES_ENV, s, t.maxMisses)
		}
	}
	if t.pollTimeout >= t.pollInterval {
		clamped := t.pollInterval / 2
		m.log.warnf("worker poll timeout %s does not fit under the poll interval %s; using %s", t.pollTimeout, t.pollInterval, clamped)
		t.pollTimeout = clamped
	}
	if t != defaultWorkerTiming {
		m.log.infof("worker timing: poll every %s (timeout %s), down after %d misses, control timeout %s, pull timeout %s, launch wait %s",
			t.pollInterval, t.pollTimeout, t.maxMisses, t.controlTimeout, t.pullTimeout, t.launchWait)
	}
	return t
}

// envDuration overrides *d from the named variable when it holds a positive
// duration; anything else is logged and leaves *d alone.
func (m *Manager) envDuration(name string, d *time.Duration) {
	s, ok := os.LookupEnv(name)
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

// Selection failures, distinguished so cmgrd can map them to 503 vs 500.
var (
	ErrAllWorkersOverloaded = errors.New("all workers are overloaded")
	ErrAllWorkersDown       = errors.New("no workers are reachable")
)

type workerHealth int32

const (
	workerDown       workerHealth = iota // sticky; only re-add clears it, once the box is back or replaced
	workerOverloaded                     // telemetry reports overloaded (or is not yet reachable)
	workerOk                             // healthy: eligible for placement
)

func (h workerHealth) String() string {
	switch h {
	case workerOk:
		return "ok"
	case workerOverloaded:
		return "overloaded"
	default:
		return "down"
	}
}

// workerConn is the runtime state for one worker: a docker client for its
// daemon, a telemetry-derived health flag, and a launch semaphore sized like
// the local one. Health only gates placement of new instances; operations on
// existing instances route through cli regardless.
type workerConn struct {
	ip     string
	public string // player-facing address (IP or hostname); "" = use ip
	cli    *client.Client
	health atomic.Int32  // holds a workerHealth
	downCh chan struct{} // closed when the worker is marked down; waiters give up at once
	queue  *daemonQueue
	done   chan struct{} // closed on removal/replacement to stop the poller
}

type WorkerInfo struct {
	IP        string `json:"ip"`
	Public    string `json:"public"`
	Health    string `json:"health"`
	Instances int    `json:"instances"`
}

// EnableWorkerPlacement turns on worker selection for new instances. Only
// cmgrd calls this; without it, configured workers are still routable for
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
		w, err := m.newWorkerConn(row.Ip, row.Public)
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

func (m *Manager) newWorkerConn(ip, public string) (*workerConn, error) {
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
		ip:     ip,
		public: public,
		cli:    cli,
		queue:  newDaemonQueue(slots(m.launchConcurrency)),
		downCh: make(chan struct{}),
		done:   make(chan struct{}),
	}
	// Fail closed for placement until the first successful poll — but as
	// overloaded, not down: down is sticky and would never recover. The
	// poller only starts once the leftovers of instances cmgr no longer
	// records on the box are gone (runWorker), so nothing is placed there
	// before their ports and network names are free.
	w.health.Store(int32(workerOverloaded))
	go m.runWorker(w)
	return w, nil
}

// runWorker reconciles the worker (reconcileWorker), then polls it
// (pollWorker). A pass that does not finish is retried at the poll cadence,
// the worker staying out of placement meanwhile, for as long as the poller
// tolerates telemetry silence (maxMisses polls). A daemon that is merely
// still starting when cmgrd starts or the worker is added therefore costs
// seconds, not a worker-add.
func (m *Manager) runWorker(w *workerConn) {
	t := m.timing()
	if m.reconcileWithRetries(w, m.reconcileWorker, time.Duration(t.maxMisses)*t.pollInterval, t.pollInterval) {
		m.pollWorker(w)
	}
}

// reconcileWithRetries runs reconcile until it comes back done, waiting
// interval between attempts, for at most budget. It reports whether the
// worker is to be polled.
//
// Past the budget the two ways of not finishing part company. A daemon that
// stayed unreachable marks the worker down, as a hung control call would.
// A pass that reached the daemon but could not finish (a database read
// failed, or the daemon refused a removal) lets the worker join placement
// anyway, loudly: its leftovers hold host ports, so some launches there will
// fail and be retried elsewhere, which is a smaller loss than holding a whole
// box out of the fleet over one container the daemon will not remove.
func (m *Manager) reconcileWithRetries(w *workerConn, reconcile func(*workerConn) reconcileResult, budget, interval time.Duration) bool {
	deadline := time.Now().Add(budget)
	for attempt := 1; ; attempt++ {
		result := reconcile(w)
		if result == reconcileDone {
			return !w.gone()
		}
		if w.gone() {
			return false
		}
		if attempt == 1 {
			m.log.warnf("worker %s: reconcile %s; retrying for up to %s before it takes placements", w.ip, result, budget)
		}
		if !time.Now().Before(deadline) {
			if result == reconcileUnreachable {
				m.log.errorf("worker %s: its daemon stayed unreachable for %s; marking the worker down", w.ip, budget)
				m.markWorkerDown(w)
				return false
			}
			m.log.errorf("worker %s: could not be fully reconciled within %s; it takes placements with leftovers that may still hold host ports", w.ip, budget)
			return true
		}
		select {
		case <-w.done:
			return false
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

// pollWorker keeps the worker's health flag current from its telemetry agent.
// Runs until the conn is removed or replaced. Down is sticky: once set (by
// telemetry silence, a docker transport failure or SetWorkerDown) only a
// manual worker-add, which rebuilds the conn and poller, recovers the worker.
// That is the operator saying the box is back: rebooted or repaired, in which
// case its instances come back with it (their containers restart on their
// own), or terminated and recreated, in which case the old entry is removed
// first (RemoveWorker) and the new box added.
// Those other two sources can mark the worker down while a poll is in flight,
// so the poller stores its verdict through pollerSetHealth, which never
// overwrites down.
func (m *Manager) pollWorker(w *workerConn) {
	timing := m.timing()
	url := fmt.Sprintf("http://%s:%d/health", w.ip, workerTelemetryPort)
	httpClient := &http.Client{Timeout: timing.pollTimeout}
	misses := 0

	update := func() {
		if workerHealth(w.health.Load()) == workerDown {
			return
		}
		overloaded, err := pollTelemetry(httpClient, url)
		if err != nil {
			misses++
			if misses >= timing.maxMisses {
				m.markWorkerDown(w)
			}
			// Below the threshold: keep the last decision to ride out a blip.
			return
		}
		misses = 0
		if overloaded {
			m.pollerSetHealth(w, workerOverloaded)
		} else {
			m.pollerSetHealth(w, workerOk)
		}
	}

	update()
	ticker := time.NewTicker(timing.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-w.done:
			return
		case <-ticker.C:
			update()
		}
	}
}

// markWorkerDown stores down, the terminal state, unconditionally. It is
// reached from telemetry silence (the poller), a docker transport failure
// and SetWorkerDown; only worker-add undoes it, by replacing the conn. The
// swap makes exactly one caller the one that moved the worker to down, which
// is the one that closes downCh, waking whatever waits on the daemon.
func (m *Manager) markWorkerDown(w *workerConn) {
	if prev := workerHealth(w.health.Swap(int32(workerDown))); prev != workerDown {
		if w.downCh != nil {
			close(w.downCh)
		}
		m.log.infof("worker %s: %s -> down", w.ip, prev)
	}
}

// pollerSetHealth stores the poller's verdict (ok or overloaded) unless the
// worker is down. A plain swap would let a poll that started before a
// worker-down or transport error land after it and flip the worker back to
// ok, so the store is a compare-and-swap against the state the poller last
// saw, retried until it either lands or finds the worker down.
func (m *Manager) pollerSetHealth(w *workerConn, h workerHealth) {
	for {
		prev := workerHealth(w.health.Load())
		if prev == workerDown || prev == h {
			return
		}
		if w.health.CompareAndSwap(int32(prev), int32(h)) {
			m.log.infof("worker %s: %s -> %s", w.ip, prev, h)
			return
		}
	}
}

// noteWorkerTransportError marks a worker down after a connection-level
// docker failure. A single hung or refused control-plane call is treated as
// worker death: the dominant real failure is an OOMed box that never comes
// back, and down being sticky means there is no flapping to guard against.
// API-level errors pass through untouched (see isTransportError).
func (m *Manager) noteWorkerTransportError(worker string, err error) {
	if worker == "" || !isTransportError(err) {
		return
	}
	m.workersMu.RLock()
	w, ok := m.workers[worker]
	m.workersMu.RUnlock()
	if !ok {
		return
	}
	m.markWorkerDown(w)
}

// SetWorkerDown marks a worker down administratively, taking it out of
// placement without touching its registry entry or instance records (unlike
// RemoveWorker, which purges both). Intended for a box about to be taken out
// of service, for a reboot, a repair or its termination: down is sticky — the
// poller treats it as terminal — so recovery is AddWorker on the same IP,
// which rebuilds the connection and poller, or RemoveWorker and AddWorker of
// the replacement.
//
// Note this also switches that worker's instances to the DB-only stop path
// (see stopInstance): their containers are no longer torn down over docker.
// The worker-add that recovers the box removes what those stops left behind.
func (m *Manager) SetWorkerDown(ip string) error {
	m.workersMu.RLock()
	w, ok := m.workers[ip]
	m.workersMu.RUnlock()
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

	w, err := m.newWorkerConn(ip, public)
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
		infos = append(infos, WorkerInfo{
			IP:        ip,
			Public:    w.public,
			Health:    workerHealth(w.health.Load()).String(),
			Instances: countMap[ip],
		})
	}
	return infos, nil
}

// WorkersConfigured reports whether any workers exist; used by cmgrd to
// decide between the legacy single-host gate and worker placement.
func (m *Manager) WorkersConfigured() bool {
	m.workersMu.RLock()
	defer m.workersMu.RUnlock()
	return len(m.workers) > 0
}

// selectWorker picks the next worker for a new instance: round robin over the
// configured workers, skipping overloaded and down ones. With no workers
// configured it returns "" (place locally). When every worker is skipped the
// error distinguishes "at least one was merely overloaded" (retryable, 503)
// from "everything is down" (500).
func (m *Manager) selectWorker() (string, error) {
	m.workersMu.Lock()
	defer m.workersMu.Unlock()

	n := len(m.workerOrder)
	if n == 0 {
		return "", nil
	}

	sawOverloaded := false
	for i := 0; i < n; i++ {
		idx := (m.rrCursor + i) % n
		w := m.workers[m.workerOrder[idx]]
		switch workerHealth(w.health.Load()) {
		case workerOk:
			m.rrCursor = (idx + 1) % n
			return w.ip, nil
		case workerOverloaded:
			sawOverloaded = true
		}
	}

	if sawOverloaded {
		return "", ErrAllWorkersOverloaded
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

// workerIsDown reports whether the instance-hosting worker at ip is
// unreachable: marked down by its poller or already purged entirely. Used by
// the stop path to skip docker teardown that cannot succeed.
func (m *Manager) workerIsDown(ip string) bool {
	m.workersMu.RLock()
	defer m.workersMu.RUnlock()
	w, ok := m.workers[ip]
	if !ok {
		return true
	}
	return workerHealth(w.health.Load()) == workerDown
}

// workerDownCh returns a channel closed once the worker is marked down, for
// waiting on alongside something else. It is nil, and so never ready, for an
// instance on the local daemon and for a worker that is already gone, whose
// callers check workerIsDown instead.
func (m *Manager) workerDownCh(worker string) <-chan struct{} {
	if worker == "" {
		return nil
	}
	m.workersMu.RLock()
	defer m.workersMu.RUnlock()
	if w, ok := m.workers[worker]; ok {
		return w.downCh
	}
	return nil
}

// instanceClient resolves the docker client for the daemon hosting the
// instance: the local env-configured client for instances with no worker,
// otherwise the worker's client. Instances on purged workers are handled by
// the stop path's short-circuit before this is reached.
func (m *Manager) instanceClient(instance *InstanceMetadata) (*client.Client, error) {
	if instance.Worker == "" {
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
// teardowns, as many of each as CMGR_CONCURRENT_LAUNCHES. Teardowns have
// slots of their own so that a deluge of stops queues here, bounded, rather
// than inside dockerd, which serializes the network side of each removal:
// left to queue there, a mass stop made the daemon slow, then unresponsive,
// and a launch's network create behind it ran into the control timeout and
// marked the healthy worker down. Two pools rather than one so that stops and
// launches do not starve each other.
type daemonQueue struct {
	launchSem   chan struct{}
	teardownSem chan struct{}
}

func newDaemonQueue(slots int) *daemonQueue {
	return &daemonQueue{
		launchSem:   make(chan struct{}, slots),
		teardownSem: make(chan struct{}, slots),
	}
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
