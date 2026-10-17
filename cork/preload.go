package cork

import (
	"context"
	"errors"
	"maps"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/image"
	"github.com/moby/moby/client"
)

// A preloaded schema has every image it launches held by every worker, so no
// launch waits on a pull: the mode for a set that fits on the workers, where
// pulling on demand (the default) suits one that does not. It covers workers
// added later, and follows redeploys: the new generation is pulled everywhere.
//
// Cleanup is opportunistic, for as long as a challenge is preloaded: after its
// pulls, each worker deletes the images of it that no current build launches
// (superseded), unless a container still uses one, which is tagged stale
// (staleTag) instead and deleted by a later pass once nothing uses it. What an
// operation takes out of preloading -- the schema's preload removed, the schema
// deleted, a challenge dropped -- may stay on the workers, and corkd lists the
// images that may remain (releasedImages) rather than track them down.

var (
	// ErrPreloadNeedsRegistry: workers preload from the challenge registry.
	ErrPreloadNeedsRegistry = errors.New("preloading needs a challenge registry (CORK_REGISTRY)")
)

// preloadSweepInterval is the pass every worker gets without being asked: the
// backstop for removals put off, and the slowest retry of a failing pass.
const preloadSweepInterval = 5 * time.Minute

// preloadRetryAfter is how soon a pass that failed goes again, doubling while
// it keeps failing (up to the sweep). A variable so tests can shorten it.
var preloadRetryAfter = 30 * time.Second

// AddPreload preloads the schema on every worker. Idempotent.
func (m *Manager) AddPreload(schema string) (err error) {
	if m.challengeRegistry == "" {
		return ErrPreloadNeedsRegistry
	}
	// Deferred first, so it runs once the lock is let go.
	defer func() {
		if err == nil {
			m.kickPreloads()
		}
	}()
	m.lockUpdates()
	defer m.unlockUpdates()

	exists, err := m.schemaExists(schema)
	if err != nil {
		return err
	}
	if !exists {
		return &UnknownIdentifierError{Type: "schema", Name: schema}
	}
	_, err = m.db.Exec("INSERT INTO preloads(schema) VALUES (?) ON CONFLICT(schema) DO NOTHING;", schema)
	return err
}

// RemovePreload returns the schema to pulling on demand. Its images stay on the
// workers, warm, and leave corkd's cleanup unless another preloaded schema
// wants them; it returns those (releasedImages), for an operator to know what
// eviction, or a hand, has to take from here.
func (m *Manager) RemovePreload(schema string) ([]string, error) {
	// Serialized with updates, so that what it reports is what it changed.
	m.lockUpdates()
	defer m.unlockUpdates()
	// Read here rather than taken from lockUpdates, which reads only while
	// it believes something is preloaded: an answer of "nothing released"
	// must mean that, not that the reading was missing.
	before, err := m.preloadSets()
	if err != nil {
		return nil, err
	}
	res, err := m.db.Exec("DELETE FROM preloads WHERE schema = ?;", schema)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, &UnknownIdentifierError{Type: "preload", Name: schema}
	}
	after, err := m.preloadSets()
	if err != nil {
		return nil, err
	}
	return releasedImages(before, after), nil
}

// PreloadInfo is one preloaded schema and how far each worker has got with it.
type PreloadInfo struct {
	Schema  string          `json:"schema"`
	Images  int             `json:"images"`
	Workers []PreloadWorker `json:"workers"`
}

type PreloadWorker struct {
	IP        string `json:"ip"`
	Reachable string `json:"reachable"`
	// Held counts the schema's images the worker was last seen to hold.
	Held     int              `json:"held"`
	Pulling  string           `json:"pulling,omitempty"`
	Failed   []PreloadFailure `json:"failed,omitempty"`
	LastPass *time.Time       `json:"last_pass,omitempty"`
}

type PreloadFailure struct {
	Image string `json:"image"`
	Error string `json:"error"`
}

// ListPreloads reports every preloaded schema, worker by worker.
func (m *Manager) ListPreloads() ([]PreloadInfo, error) {
	sets, err := m.preloadSets()
	if err != nil {
		return nil, err
	}

	type snapshot struct {
		ip        string
		reachable workerReachable
		status    *preloadStatus
	}
	m.workersMu.RLock()
	snaps := make([]snapshot, 0, len(m.workerOrder))
	for _, ip := range m.workerOrder {
		w := m.workers[ip]
		snaps = append(snaps, snapshot{ip, workerReachable(w.reachable.Load()), &w.preload})
	}
	m.workersMu.RUnlock()

	infos := []PreloadInfo{}
	for _, schema := range sets.schemaNames() {
		wanted := sets.bySchema[schema]
		info := PreloadInfo{Schema: schema, Images: len(wanted), Workers: []PreloadWorker{}}
		for _, snap := range snaps {
			info.Workers = append(info.Workers, snap.status.report(snap.ip, snap.reachable, wanted))
		}
		infos = append(infos, info)
	}
	return infos, nil
}

// preloadStatus is what a worker's preloader last established. A replaced conn
// starts empty and its first pass fills it in.
type preloadStatus struct {
	mu       sync.Mutex
	held     map[string]bool   // images the worker holds
	failed   map[string]string // wanted images whose last pull failed, and why
	pulling  string
	lastPass time.Time
}

func (s *preloadStatus) begin(held map[string]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.held = maps.Clone(held)
	s.failed = map[string]string{}
}

func (s *preloadStatus) pull(image string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pulling = image
}

func (s *preloadStatus) pulled(image string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pulling = ""
	if err != nil {
		s.failed[image] = err.Error()
		return
	}
	s.held[image] = true
}

func (s *preloadStatus) removed(image string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.held, image)
}

func (s *preloadStatus) finish() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pulling = ""
	s.lastPass = time.Now()
}

func (s *preloadStatus) report(ip string, reachable workerReachable, wanted []string) PreloadWorker {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := PreloadWorker{IP: ip, Reachable: reachable.String()}
	for _, image := range wanted {
		if image == s.pulling {
			r.Pulling = image // only this schema's: another's pull says nothing of it
		}
		if s.held[image] {
			r.Held++
		}
		if e, ok := s.failed[image]; ok {
			r.Failed = append(r.Failed, PreloadFailure{Image: image, Error: e})
		}
	}
	if !s.lastPass.IsZero() {
		at := s.lastPass
		r.LastPass = &at
	}
	return r
}

// kickPreloads asks every worker's preloader for a pass now.
func (m *Manager) kickPreloads() {
	m.workersMu.RLock()
	defer m.workersMu.RUnlock()
	for _, w := range m.workers {
		select {
		case w.preloadNow <- struct{}{}:
		default:
		}
	}
}

// preloadLoop runs a worker's passes, from its admission until its conn is
// replaced or removed.
func (m *Manager) preloadLoop(w *workerConn) {
	sweep := time.NewTicker(preloadSweepInterval)
	defer sweep.Stop()
	retryIn := preloadRetryAfter
	for {
		// A failed pass goes again soon, getting the new images out being
		// what comes first, but less often while it keeps failing.
		var retry <-chan time.Time
		var timer *time.Timer
		if m.preloadPass(w) {
			timer = time.NewTimer(retryIn)
			retry = timer.C
			retryIn = min(2*retryIn, preloadSweepInterval)
		} else {
			retryIn = preloadRetryAfter
		}
		select {
		case <-w.done:
		case <-w.preloadNow:
		case <-sweep.C:
		case <-retry:
		}
		if timer != nil {
			timer.Stop()
		}
		if w.gone() {
			return
		}
	}
}

// preloadPass pulls what the worker lacks, one image at a time so a pass under
// live traffic takes one core for extraction rather than every one, then
// removes what the preloaded schemas have superseded. It reports whether
// anything failed that a pass soon should try again.
func (m *Manager) preloadPass(w *workerConn) (failed bool) {
	if !m.preloadable(w) {
		return false
	}
	// No lock: what lands while the pass runs kicks the next one.
	sets, err := m.preloadSets()
	if err != nil {
		m.log.errorf("worker %s: could not read what to preload: %s", w.ip, err)
		return true
	}
	if len(sets.schemas) == 0 {
		return false
	}
	images, err := m.workerImages(w)
	if err != nil {
		return true
	}
	defer w.preload.finish()
	held := heldTags(images)
	w.preload.begin(held)

	pulls := missing(sets.wanted, held)
	// Each worker in its own order, so that a fleet starting together does
	// not ask the registry for the same image all at once.
	m.shuffle(pulls)
	pullTimeout := m.restartLimits().pullTimeout
	pulled, pullsFailed := 0, 0
	tried := map[string]bool{}
	for _, name := range pulls {
		release, ok := m.acquirePreloadPull(w)
		if !ok {
			return false
		}
		// Checked with the slot held, since waiting for one may take long.
		if !m.preloadable(w) {
			release()
			m.log.warnf("worker %s: preload pass stopped after %d pull(s): the worker is no longer reachable", w.ip, pulled+pullsFailed)
			return false
		}
		// A pass is long: its preload may have been removed since, or the
		// image superseded. Unread, nothing is pulled, and the pass goes again.
		if now, err := m.preloadSets(); err != nil || !now.wanted[name] {
			release()
			if err != nil {
				m.log.errorf("worker %s: could not read what to preload: %s", w.ip, err)
				return true
			}
			continue
		}
		w.preload.pull(name)
		err := m.pullImage(w.cli, name, pullTimeout)
		release()
		tried[name] = true
		if err != nil {
			pullsFailed++
			m.notePreloadError(w, err)
		} else {
			pulled++
		}
		w.preload.pulled(name, err)
	}
	if pulled+pullsFailed > 0 {
		m.log.infof("worker %s: preloaded %d image(s), %d failed", w.ip, pulled, pullsFailed)
	}
	removalsUnread := m.removeSuperseded(w, held, tried)
	return pullsFailed > 0 || removalsUnread
}

// defaultPreloadPulls is how many preload pulls run at once across the fleet
// (CORK_PRELOAD_PULLS): each worker pulls one at a time, and a fleet starting
// together would otherwise put every worker on the registry at once.
const defaultPreloadPulls = 8

func (m *Manager) preloadPullSlots() chan struct{} {
	n := defaultPreloadPulls
	m.envCount(PRELOAD_PULLS_ENV, &n)
	return make(chan struct{}, n)
}

// acquirePreloadPull takes one of the fleet's preload pull slots, unless the
// conn goes first. A Manager built without them (tests) has no cap.
func (m *Manager) acquirePreloadPull(w *workerConn) (release func(), ok bool) {
	if m.preloadPulls == nil {
		return func() {}, true
	}
	select {
	case m.preloadPulls <- struct{}{}:
		return func() { <-m.preloadPulls }, true
	case <-w.done:
		return nil, false
	}
}

func (m *Manager) shuffle(names []string) {
	m.randMu.Lock()
	defer m.randMu.Unlock()
	if m.rand != nil {
		m.rand.Shuffle(len(names), func(i, j int) { names[i], names[j] = names[j], names[i] })
	}
}

func (m *Manager) preloadable(w *workerConn) bool {
	return !w.gone() && workerReachable(w.reachable.Load()) == workerReachableOk
}

// removeSuperseded deletes, of the tags the worker held, those in preloaded
// challenges' repositories that no current build launches. The delete is not
// forced, so dockerd refuses it while a container uses the image, which is
// tagged stale instead (markStale). Read afresh, without a lock: at worst a
// rollback that lands meanwhile has an unused image deleted, which the next
// launch or pass pulls back. It reports whether it could not read the
// database, which the pass retries; a failed delete waits for the sweep, as
// deletes come last and the usual failure is a busy daemon.
func (m *Manager) removeSuperseded(w *workerConn, held, tried map[string]bool) (unread bool) {
	// held is the pass's listing, tried what it pulled or failed to: a tag
	// gone since is not found, which counts as removed, and one that came
	// since waits for a later pass.
	sets, err := m.preloadSets()
	if err != nil {
		m.log.errorf("worker %s: could not read what preloading supersedes: %s", w.ip, err)
		return true
	}
	for _, tag := range superseded(sets, held, tried) {
		if !m.preloadable(w) {
			return false
		}
		err := m.removeWorkerImage(w, tag)
		if errdefs.IsConflict(err) {
			err = m.markStale(w, tag)
		} else if err == nil {
			w.preload.removed(tag)
			m.log.infof("worker %s: removed superseded image %s", w.ip, tag)
		} else {
			m.log.warnf("worker %s: could not remove superseded image %s: %s", w.ip, tag, err)
		}
		if errors.Is(err, context.DeadlineExceeded) {
			// The daemon is slow; more deletes would only queue behind it.
			m.log.warnf("worker %s: its other removals wait for a later pass", w.ip)
			return false
		}
	}
	return false
}

// staleTagPrefix marks a superseded image a container still used when corkd
// went to delete it, for a later pass, an operator or a reaper to delete once
// nothing does. A reference filter's * stops at a /, so list them with
// docker images --format '{{.Repository}}:{{.Tag}}' | grep ':stale-'.
const staleTagPrefix = "stale-"

func staleTag(ref string) string {
	repo := imageRepo(ref)
	return repo + ":" + staleTagPrefix + strings.TrimPrefix(ref, repo+":")
}

func isStale(ref string) bool {
	return strings.HasPrefix(strings.TrimPrefix(ref, imageRepo(ref)+":"), staleTagPrefix)
}

// markStale gives an image in use the stale tag in place of its superseded
// one, which, with another tag on the image, a delete only takes off. Both are
// reference-store writes, quick whatever the image's size.
func (m *Manager) markStale(w *workerConn, tag string) error {
	if isStale(tag) {
		return nil // still in use; a later pass tries again
	}
	stale := staleTag(tag)
	ctx, cancel := m.controlCtx()
	_, err := w.cli.ImageTag(ctx, client.ImageTagOptions{Source: tag, Target: stale})
	cancel()
	if err == nil {
		err = m.removeWorkerImage(w, tag)
	} else {
		m.notePreloadError(w, err)
	}
	if err != nil {
		m.log.warnf("worker %s: superseded image %s is still in use, and could not be marked %s: %s", w.ip, tag, stale, err)
		return err
	}
	w.preload.removed(tag)
	m.log.infof("worker %s: superseded image %s is still in use; kept as %s, for a later pass to remove once nothing uses it", w.ip, tag, stale)
	return nil
}

// lockUpdates takes updateMu for an operation that changes builds and, while
// anything is preloaded, notes what preloading covers before it
// (unlockUpdates). Every caller goes through it.
func (m *Manager) lockUpdates() {
	m.updateMu.Lock()
	m.updateBefore = nil
	if m.preloading.Load() {
		if before, err := m.preloadSets(); err == nil {
			m.updateBefore = before
		}
	}
}

// unlockUpdates ends such an operation: it logs what preloading no longer
// covers, then releases updateMu. It kicks no one: the builds an operation
// lands kick the workers as they do.
func (m *Manager) unlockUpdates() {
	after, err := m.preloadSets()
	if err != nil {
		m.log.errorf("could not read what is preloaded after an update: %s", err)
	} else {
		m.preloading.Store(len(after.schemas) > 0)
		if m.updateBefore != nil {
			if released := releasedImages(m.updateBefore, after); len(released) > 0 {
				m.log.warnf("preloading no longer covers %d image(s), which may remain on the workers, as may older ones of their repositories, until eviction or an operator takes them: %s",
					len(released), strings.Join(released, ", "))
			}
		}
	}
	m.updateBefore = nil
	m.updateMu.Unlock()
}

// releasedImages lists the images preloading wanted before an operation that it
// neither wants nor removes after it. Read from the database alone, not from
// any worker: a worker may hold fewer of them, and older ones of the same
// repositories that removal had yet to reach.
func releasedImages(before, after *preloadSets) []string {
	released := []string{}
	for name := range before.wanted {
		if after.wanted[name] || (!after.named[name] && after.repos[imageRepo(name)]) {
			continue
		}
		released = append(released, name)
	}
	sort.Strings(released)
	return released
}

// notePreloadError passes a failed preload call to the worker's health only
// when its daemon is gone (refused, reset). A timeout is the daemon busy with
// the launches it serves, often behind preloading's own delete, and counting
// it as a stall could eject a worker that is serving.
func (m *Manager) notePreloadError(w *workerConn, err error) {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrPullTimeout) {
		return
	}
	m.noteWorkerTransportError(w.ip, w.cli, err)
}

// notePreloading reads whether any schema is preloaded (m.preloading), for a
// Manager starting up; operations keep it current from there (unlockUpdates).
func (m *Manager) notePreloading() error {
	var n int
	if err := m.db.Get(&n, "SELECT COUNT(1) FROM preloads;"); err != nil {
		return err
	}
	m.preloading.Store(n > 0)
	return nil
}

func (m *Manager) workerImages(w *workerConn) ([]image.Summary, error) {
	ctx, cancel := m.controlCtx()
	defer cancel()
	list, err := w.cli.ImageList(ctx, client.ImageListOptions{})
	if err != nil {
		m.log.errorf("worker %s: could not list images to preload: %s", w.ip, err)
		m.notePreloadError(w, err)
		return nil, err
	}
	return list.Items, nil
}

func (m *Manager) removeWorkerImage(w *workerConn, ref string) error {
	ctx, cancel := m.controlCtx()
	defer cancel()
	_, err := w.cli.ImageRemove(ctx, ref, client.ImageRemoveOptions{PruneChildren: true})
	if errdefs.IsNotFound(err) {
		return nil
	}
	if err != nil {
		m.notePreloadError(w, err)
	}
	return err
}

// preloadSets is what the database says preloading involves. Image names are
// as instanceImageName gives them, which is how a worker lists what it pulled.
type preloadSets struct {
	schemas  map[string]bool
	bySchema map[string][]string // schema -> images it preloads, sorted
	wanted   map[string]bool     // every image a preloaded schema launches
	named    map[string]bool     // every image any current build launches
	repos    map[string]bool     // repositories of preloaded challenges
}

func (s *preloadSets) schemaNames() []string {
	names := make([]string, 0, len(s.schemas))
	for name := range s.schemas {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (m *Manager) preloadSets() (*preloadSets, error) {
	sets := &preloadSets{
		schemas:  map[string]bool{},
		bySchema: map[string][]string{},
		wanted:   map[string]bool{},
		named:    map[string]bool{},
		repos:    map[string]bool{},
	}
	var schemas []string
	if err := m.db.Select(&schemas, "SELECT schema FROM preloads;"); err != nil {
		return nil, err
	}
	if len(schemas) == 0 {
		return sets, nil
	}
	for _, s := range schemas {
		sets.schemas[s] = true
		sets.bySchema[s] = []string{}
	}

	// Built rows only: an unbuilt one launches nothing, and one whose rebuild
	// failed still serves the generation it records.
	var rows []struct {
		Schema        string      `db:"schema"`
		Challenge     ChallengeId `db:"challenge"`
		Seed          int         `db:"seed"`
		Checksum      uint32      `db:"checksum"`
		Host          string      `db:"host"`
		ChallengeType string      `db:"challengetype"`
		PortCount     int         `db:"portcount"`
	}
	err := m.db.Select(&rows, `SELECT b.schema, b.challenge, b.seed, b.checksum, i.host, c.challengetype,
			(SELECT COUNT(*) FROM portNames WHERE portNames.challenge = b.challenge) AS portcount
		FROM builds AS b
		JOIN images AS i ON i.build = b.id
		JOIN challenges AS c ON c.id = b.challenge
		WHERE b.checksum != 0 AND b.flag != '' AND i.host != 'builder';`)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		name := m.instanceImageName(r.Challenge, &BuildMetadata{Seed: r.Seed, Checksum: r.Checksum}, Image{Host: r.Host})
		sets.named[name] = true
		if !sets.schemas[r.Schema] || deriveDeliveryType(r.ChallengeType, r.PortCount) != DeliveryService {
			continue
		}
		sets.wanted[name] = true
		sets.bySchema[r.Schema] = append(sets.bySchema[r.Schema], name)
		sets.repos[m.imageRepository(r.Challenge)] = true
	}
	for s := range sets.bySchema {
		sort.Strings(sets.bySchema[s])
	}
	return sets, nil
}

// heldTags is every tag a worker listed.
func heldTags(images []image.Summary) map[string]bool {
	held := map[string]bool{}
	for _, img := range images {
		for _, tag := range img.RepoTags {
			held[tag] = true
		}
	}
	return held
}

// missing lists, sorted, the wanted images not in have.
func missing(wanted, have map[string]bool) []string {
	var names []string
	for name := range wanted {
		if !have[name] {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// superseded lists, sorted, the held tags in a preloaded challenge's repository
// that no current build launches, preloaded or not. A repository waits while
// one of its wanted images landed too late for the pass to try (the next pass
// pulls it first): deleting the old version before would take the layers the
// two share. A failed pull holds nothing back, since the delete may be what
// makes room for it.
func superseded(sets *preloadSets, held, tried map[string]bool) []string {
	waiting := map[string]bool{}
	for name := range sets.wanted {
		if !held[name] && !tried[name] {
			waiting[imageRepo(name)] = true
		}
	}
	var tags []string
	for tag := range held {
		if repo := imageRepo(tag); !sets.named[tag] && sets.repos[repo] && !waiting[repo] {
			tags = append(tags, tag)
		}
	}
	sort.Strings(tags)
	return tags
}

// imageRepo strips the tag from an image name; a registry port is not a tag.
func imageRepo(name string) string {
	if i := strings.LastIndex(name, ":"); i > strings.LastIndex(name, "/") {
		return name[:i]
	}
	return name
}
