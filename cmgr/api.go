package cmgr

import (
	"errors"
	"fmt"
	"math/rand"
	"os"
	"time"

	"github.com/CyLabAcademy/challenge-orchestrator/cmgr/dockerfiles"
)

const manualSchemaPrefix = "manual-"

var version string

// Returns the version string associated with the build (results of
// `git describe --tags`) or "unknown" if it was not set at build time.
func Version() string {
	if version != "" {
		return version
	}
	return "unknown"
}

// Creates a new instance of the challenge manager validating the appropriate
// environment variables in the process.  A return value of `nil` indicates
// a fatal error occurred during intitialization.
func NewManager(logLevel LogLevel) *Manager {
	mgr := new(Manager)
	mgr.log = newLogger(logLevel)
	mgr.rand = rand.New(rand.NewSource(time.Now().UnixNano()))

	mgr.log.infof("version: %s", Version())

	if err := mgr.initPolicy(); err != nil {
		mgr.log.error(err)
		return nil
	}

	// Before the directories, the pins and docker: on an external build
	// plane each of those skips what only a local one reads.
	if err := mgr.initBuildPlane(); err != nil {
		return nil
	}

	mgr.workerTiming = mgr.workerTimingFromEnv()

	if err := mgr.setDirectories(); err != nil {
		return nil
	}

	if err := mgr.initBasePins(); err != nil {
		return nil
	}

	if err := mgr.initDocker(); err != nil {
		return nil
	}

	if err := mgr.initDatabase(); err != nil {
		return nil
	}

	if err := mgr.checkExternalBuildPlane(); err != nil {
		return nil
	}

	if err := mgr.initWorkers(); err != nil {
		return nil
	}

	mgr.pruneInterval = 1 * time.Minute
	pruneAgeStr, isSet := os.LookupEnv(PRUNE_AGE_ENV)
	if !isSet {
		mgr.pruneAge = 1 * time.Hour
	} else {
		age, err := time.ParseDuration(pruneAgeStr)
		if err != nil {
			mgr.log.errorf("invalid prune age '%s': %s", pruneAgeStr, err)
			mgr.pruneAge = 1 * time.Hour
		} else {
			mgr.pruneAge = age
		}
	}

	return mgr
}

// Traverses the entire directory and captures all valid challenge
// descriptions it comes across.  In general, it will continue even when it
// encounters errors (permission, poorly formatted JSON, etc.) in order to
// give the as much feedback as possible to the caller.  However, it will fail
// fast on two challenges with the same name and namespace.
//
// This function does not have any side-effects on the database or
// built/running challenge state, but changes that it detects will effect new
// builds.  It is important to resolve any issues/errors it raises before
// making any other API calls for affected challenges.  Failure to follow this
// guidance could result in inconsistencies in deployed challenges.
func (m *Manager) DetectChanges(fp string) *ChallengeUpdates {
	cu := new(ChallengeUpdates)

	// No tree to scan: drift is detected where the tree is, on the build
	// plane, and this daemon learns of a change when the build arrives.
	if m.externalBuildPlane {
		cu.Errors = []error{ErrExternalBuildPlane}
		return cu
	}

	if fp == "" {
		fp = m.chalDir
	}

	fp, err := m.normalizeDirPath(fp)
	if err != nil {
		cu.Errors = []error{err}
		return cu
	}

	challenges, errs := m.inventoryChallenges(fp)
	db_metadata, err := m.listChallenges()

	if err != nil {
		cu.Errors = append(errs, err)
		return cu
	}

	// One query for every challenge with a build left at an earlier
	// generation, rather than one per unchanged challenge below; a probe
	// that fails is an error of the scan, not a clean tree.
	stale, err := m.staleChallengeSet()
	if err != nil {
		errs = append(errs, err)
	}

	for _, curr := range db_metadata {
		newMeta, ok := challenges[curr.Id]
		if !ok {
			if pathInDirectory(curr.Path, fp) || !pathInDirectory(curr.Path, m.chalDir) {
				cu.Removed = append(cu.Removed, curr)
			}
			continue
		}

		sourceChanged := curr.SourceChecksum != newMeta.SourceChecksum
		metadataChanged := curr.MetadataChecksum != newMeta.MetadataChecksum
		solvescriptChanged := curr.SolveScript != newMeta.SolveScript
		// safeToRefresh is a full metadata lookup, so it is asked only where
		// its answer can change the verdict: never for a source change.
		switch {
		case sourceChanged:
			cu.Updated = append(cu.Updated, newMeta)
		case metadataChanged || solvescriptChanged:
			if m.safeToRefresh(newMeta) {
				m.log.debugf("Marking %s as refresh", newMeta.Id)
				cu.Refreshed = append(cu.Refreshed, newMeta)
			} else {
				cu.Updated = append(cu.Updated, newMeta)
			}
		case !m.safeToRefresh(newMeta):
			// The checksums are unchanged but the persisted options disagree
			// with what the current loader parses — e.g. a challenge that
			// declared a seccomp profile before the binary understood the
			// option, or a corrupt options row. Re-persist through the
			// refresh path (no rebuild) so the declared options take effect.
			m.log.infof("Marking %s as refresh: persisted options differ from parsed metadata", newMeta.Id)
			cu.Refreshed = append(cu.Refreshed, newMeta)
		case stale[curr.Id]:
			// Nothing changed on disk, but a build was produced from an
			// earlier generation: its last rebuild failed after the challenge
			// row had moved on (updateChallenges commits the metadata before
			// it builds). Compared by checksums alone the challenge looks
			// unmodified and the failure would be invisible to every later
			// update; it stays reported, and rebuilt, until a rebuild
			// succeeds. A challenge that is also Refreshed is reported as
			// that, and its stale builds are rebuilt on that path all the same.
			cu.Stale = append(cu.Stale, newMeta)
		default:
			cu.Unmodified = append(cu.Unmodified, curr)
		}
		delete(challenges, curr.Id)
	}

	for _, metadata := range challenges {
		cu.Added = append(cu.Added, metadata)
	}

	cu.Errors = errs
	return cu
}

// UpdateOptions adjusts the behavior of `UpdateWithOptions`.
type UpdateOptions struct {
	// PruneOldImages untags the image generation a rebuild displaces from its
	// rollback slot: each build keeps its new images plus the generation it
	// just replaced (the rollback target, see BuildMetadata.PrevChecksum), and
	// the one pushed beyond that is untagged after all of the challenge's builds
	// are processed. Only generations displaced by THIS update are removed —
	// generations orphaned by earlier updates that ran without this flag are
	// not tracked and are not reclaimed here. Off by default: a tag can be
	// referenced outside this cmgr database (e.g. another cmgr instance on the
	// same docker daemon that built identical content), and cmgr cannot see
	// those references.
	PruneOldImages bool
}

// This will update the global system state based off the changes that are
// detected by a call to `DetectChanges`.  Specifically, in addition to
// updating challenge metadata (new and existing) it will rebuild and, if
// successful restart, existing challenges and then remove the metadata for
// challenges that can no longer be found.  Challenges that have not been
// modified should not be affected.
//
// In the presence of errors, this function will do addition and updates as
// best it can in order to preserve a consistent system state.  If a build
// fails, the build keeps its previous generation (row, images, instances)
// while the challenge metadata is already updated; the challenge is then
// reported as Stale by every later DetectChanges, and rebuilt by every later
// update, until a rebuild succeeds.  Additionally, in the presence of errors
// it will not perform any removals of challenge metadata (removing a built
// challenge is considered an error).
func (m *Manager) Update(fp string) *ChallengeUpdates {
	return m.UpdateWithOptions(fp, UpdateOptions{})
}

// UpdateWithOptions is identical to Update but takes explicit options; Update
// is equivalent to calling this with the zero-value UpdateOptions.
func (m *Manager) UpdateWithOptions(fp string, options UpdateOptions) *ChallengeUpdates {
	// Refused before the lock: a schema operation waiting behind it would
	// otherwise queue on a request that can do nothing.
	if m.externalBuildPlane {
		return &ChallengeUpdates{Errors: []error{ErrExternalBuildPlane}}
	}

	// One rebuild at a time. Two of them would work over the same instances:
	// each tearing down what the other just started, reassigning the same
	// ports twice, and taking the network the other had just created for a
	// leftover of an earlier generation (startNetwork). An update is an
	// operator action, so the second one waits rather than being refused.
	m.updateMu.Lock()
	defer m.updateMu.Unlock()

	cu := m.DetectChanges(fp)
	errs := m.addChallenges(cu.Added)
	if len(errs) != 0 {
		cu.Errors = append(cu.Errors, errs...)
	}

	// A source change rebuilds every build of the challenge; the other two
	// persisted verdicts rebuild only what an earlier rebuild left at a
	// previous generation, which for a Refreshed challenge is normally
	// nothing. The buckets are disjoint, so the two share one pass.
	leftovers := append(append([]*ChallengeMetadata{}, cu.Refreshed...), cu.Stale...)
	cu.Errors = append(cu.Errors, m.updateChallenges(leftovers, m.staleBuildIds, options.PruneOldImages)...)
	cu.Errors = append(cu.Errors, m.updateChallenges(cu.Updated, m.allBuildIds, options.PruneOldImages)...)

	if len(cu.Errors) == 0 {
		err := m.removeChallenges(cu.Removed)
		if err != nil {
			cu.Errors = append(cu.Errors, err)
		}
	}
	return cu
}

// Templates out a "challenge" and generates concrete images, flags, and
// lookup values for the seeds provided which is called a "build" and returns
// a list of identifiers that can be used to reference the build in other API
// functions.  This function may take a significant amount of time because it
// will implicitly download base docker images and build the artifacts.
//
// NOTE: if `CMGR_REGISTRY` is specified, the generation this build displaces
// is offered to BuildKit as a cache source and the image produced carries
// inline cache metadata, so a builder whose local cache was reclaimed recovers
// the shared layers from the registry rather than re-running every install.
// See cacheRefsFor and BUILDER.md. (This replaced a per-challenge "freeze"
// image, which is gone.)
func (m *Manager) Build(challenge ChallengeId, seeds []int, flagFormat string) ([]*BuildMetadata, error) {
	if m.externalBuildPlane {
		return nil, ErrExternalBuildPlane
	}
	schema := fmt.Sprintf("%s%x", manualSchemaPrefix, m.rand.Int63())
	instanceCount := -1

	builds := make([]*BuildMetadata, len(seeds))
	for i := range builds {
		builds[i] = &BuildMetadata{
			Seed:          seeds[i],
			Format:        flagFormat,
			Challenge:     challenge,
			Schema:        schema,
			InstanceCount: instanceCount,
		}
	}
	err := m.generateBuilds(builds)
	return builds, err
}

// Creates a running "instance" of the given build and returns its identifier
// on success otherwise an error. The optional envVars map provides extra
// environment variables to inject into the instance's containers. Keys in
// envVars should already include the "CMGR_" prefix. Pass nil if no
// additional environment variables are needed.
func (m *Manager) Start(build BuildId, envVars map[string]string) (InstanceId, error) {
	// Get build metadata
	//
	// This read is the launch's whole view of the build. A rebuild that
	// stamps a new checksum after it, and takes its snapshot of the build's
	// instances before this launch inserts its row, neither sees this
	// instance nor is seen by it: the instance serves the generation that was
	// current when it began, under a build that now records the next one. It
	// is left to expire rather than torn down. The flag does not change with
	// a rebuild, the superseded images stay in retention as the rollback
	// target, and only update-schema creates persistent instances, so this is
	// bounded to one on-demand instance and one TTL. Serializing against a
	// rebuild is not an option: a rebuild runs for minutes and every caller
	// blocks on its launch.
	bMeta, err := m.lookupBuildMetadata(build)
	if err != nil {
		return 0, err
	}

	if bMeta.InstanceCount != DYNAMIC_INSTANCES {
		return 0, errors.New("locked build: change the schema definition to start more instances")
	}

	return m.newInstance(bMeta, envVars, m.requestLimits())
}

// newInstance records and launches an instance of the build on the daemon
// placement picks, within limits: the request limits for a launch the
// platform asked for, the restart limits for one the schema converge creates,
// since nothing retries the latter.
func (m *Manager) newInstance(build *BuildMetadata, envVars map[string]string, limits launchLimits) (InstanceId, error) {
	cMeta, err := m.GetChallengeMetadata(build.Challenge)
	if err != nil {
		return 0, err
	}

	if !cMeta.NeedsInstance() {
		return 0, fmt.Errorf("challenge %s is %s: its builds are delivered without instances", cMeta.Id, cMeta.DeliveryType)
	}

	iMeta := &InstanceMetadata{
		Build:      build.Id,
		Ports:      make(map[string]int),
		Containers: []string{},
	}

	// Placement (cmgrd only): pick a worker before the instance row is
	// created so the worker is recorded with it. With no workers configured
	// selectWorker returns "" and the instance runs on the local daemon --
	// on a local build plane. An external one has no local daemon, so a
	// launch with nowhere to go is refused here, before anything is
	// recorded, as a failure of the fleet rather than a retryable one.
	if m.placementEnabled {
		worker, err := m.selectWorker()
		if err != nil {
			return 0, err
		}
		iMeta.Worker = worker
	}
	if iMeta.Worker == "" && m.externalBuildPlane {
		return 0, ErrNoWorkers
	}

	// Refuse at once what would only be refused after the wait (admit).
	if err := m.admit(iMeta, limits); err != nil {
		return 0, err
	}

	err = m.openInstance(iMeta)
	if err != nil {
		return 0, err
	}

	m.checkPrune()

	revPortMap, err := m.getReversePortMap(build.Challenge)
	if err != nil {
		m.clearInstanceRecords(iMeta.Id, "its port map could not be read")
		return 0, err
	}

	if m.portLow != 0 {
		for _, image := range build.Images {
			if image.Host == "builder" {
				continue
			}
			for _, portStr := range image.Ports {
				portName := revPortMap[portStr]
				hostPort, err := m.reservePort(iMeta.Id, iMeta.Worker, portName)
				if err != nil {
					// Nothing has reached the daemon: clear the records (port
					// rows cascade) without a docker round trip.
					m.clearInstanceRecords(iMeta.Id, "a port could not be reserved")
					return 0, err
				}
				iMeta.Ports[portName] = hostPort
			}
		}
	}

	started, err := m.launch(build, iMeta, cMeta.ChallengeOptions.NetworkOptions, cMeta.ChallengeOptions.Overrides, envVars, revPortMap, limits)
	if err != nil && started {
		// It is possible we are in a partially deployed state.  Make sure
		// we are torn down, but ignore the returned error.
		m.stopInstance(iMeta)
	} else if err != nil {
		// Nothing reached the daemon: clear the records (port and container
		// rows cascade) without a docker round trip, which against a wedged
		// daemon would hold the retryable answer for another control timeout.
		m.clearInstanceRecords(iMeta.Id, "its launch was refused before it reached the daemon")
	}

	return iMeta.Id, err
}

// clearInstanceRecords removes the records of an instance whose launch failed
// before anything of it reached a daemon; its port and container rows cascade.
// A failure here is logged rather than returned: the request is already
// failing, and the row, never finalized, is reclaimed by the sweep in Prune a
// few minutes later, so it holds its ports until then but not for good.
func (m *Manager) clearInstanceRecords(id InstanceId, why string) {
	if err := m.removeInstanceMetadata(id); err != nil {
		m.log.errorf("could not clear the records of instance %d after %s: %s; the unfinalized-instance sweep reclaims them", id, why, err)
	}
}

// Stops the running "instance".
func (m *Manager) Stop(instance InstanceId) error {
	// Get instance metadata
	iMeta, err := m.lookupInstanceMetadata(instance)
	if err != nil {
		return err
	}

	// Get build metadata
	bMeta, err := m.lookupBuildMetadata(iMeta.Build)
	if err != nil {
		return err
	}

	if bMeta.InstanceCount != DYNAMIC_INSTANCES {
		return errors.New("locked build: change the schema definition to stop this instance")
	}
	return m.stopInstance(iMeta)
}

// unreachable reports an instance no daemon of this process can reach: one
// on a down (or purged) worker, or one placed on the local daemon, which an
// external build plane does not have. Both are stopped by clearing records
// alone (stopInstance); instanceClient refuses the latter outright.
func (m *Manager) unreachable(instance *InstanceMetadata) bool {
	if instance.Worker == "" {
		return m.externalBuildPlane
	}
	return m.workerIsDown(instance.Worker)
}

// whereUnreachable says why an instance is out of reach, for the log.
func (m *Manager) whereUnreachable(instance *InstanceMetadata) string {
	if instance.Worker == "" {
		return "placed on the local daemon, which an external build plane has none of"
	}
	return fmt.Sprintf("worker %s down", instance.Worker)
}

func (m *Manager) stopInstance(instance *InstanceMetadata) error {
	// A daemon that cannot be reached -- a down (or purged) worker, or the
	// local daemon an external build plane does not have: clear our records
	// and report success so callers (the platform's stop/restart/TTL flows,
	// a schema delete, a prune) are never wedged behind it. Any containers
	// actually left running on a worker are removed if the box rejoins
	// placement (reconcileWorker, on worker-add and at startup); until then
	// they are docker-reaper's.
	if m.unreachable(instance) {
		m.log.warnf("%s: clearing instance %d records without docker teardown", m.whereUnreachable(instance), instance.Id)
		return m.removeInstanceMetadata(instance.Id)
	}

	err := m.teardown(instance)
	if err != nil {
		// The teardown itself took the worker down (a control call hung), or
		// the worker went down while it waited for its slot: finish the way a
		// stop on a down worker does, so the caller gets its success now
		// rather than from a second attempt.
		if m.unreachable(instance) {
			m.log.warnf("worker %s went down during the stop of instance %d: clearing its records without further docker teardown", instance.Worker, instance.Id)
			return m.removeInstanceMetadata(instance.Id)
		}
		return err
	}

	return m.removeInstanceMetadata(instance.Id)
}

// Destroys the assoicated "build".
func (m *Manager) Destroy(build BuildId) error {
	// Get build metadata
	bMeta, err := m.lookupBuildMetadata(build)
	if err != nil {
		return err
	}

	if bMeta.Schema[:len(manualSchemaPrefix)] != manualSchemaPrefix {
		return errors.New("locked build: change the schema definition to destroy this build")
	}

	return m.destroyImages(build)
}

// Obtains a list of challenges with minimal version information filled into
// the metadata object.
func (m *Manager) ListChallenges() []*ChallengeMetadata {
	md, _ := m.listChallenges()
	return md
}

// Obtains a list of challenges which match on all of the given tags.  If no
// tags are passed, then it returns the same results as `ListChallenges`.
// Wildcards are allowed as either '*' or '%' and the search is ASCII case
// insensitive.
func (m *Manager) SearchChallenges(tags []string) []*ChallengeMetadata {
	md, _ := m.searchChallenges(tags)
	return md
}

// Lists all schemas as currently defined in the database.
func (m *Manager) ListSchemas() ([]string, error) {
	return m.queryForSchemas()
}

// Uses the schema as a definition of builds and instances that should be
// created/started.  Prevents management of those builds and instances from
// other API calls unless explicitly allowed by the schema.  This call is
// likely to be extremely time and resource intensive as it will start creating
// all of the requested builds immediately and not return until complete.
//
// Schema operations share updateMu with rebuilds (UpdateWithOptions): a
// converge builds, stops and launches instances of the same builds an update
// rebuilds and restarts, and the two interleaved would work over each other's
// instances just as two updates would. The lock is taken here, at the API
// boundary, and never inside: the unlocked helpers below are what the
// operations call each other through.
func (m *Manager) CreateSchema(schema *Schema) []error {
	m.updateMu.Lock()
	defer m.updateMu.Unlock()
	return m.createSchema(schema)
}

// createSchema is CreateSchema without the lock; the caller holds updateMu.
func (m *Manager) createSchema(schema *Schema) []error {
	exists, err := m.schemaExists(schema.Name)
	if err != nil {
		return []error{err}
	} else if exists {
		return []error{fmt.Errorf("schema '%s' already exists", schema.Name)}
	}

	return m.convergeSchema(schema)
}

// Updates the definition of the schema internally and then converges to the
// new definition.  Certain updates are more expensive than others.  In
// particular, updating the flag format will cause a complete rebuild of the
// state.  Serialized with rebuilds and other schema operations; see
// CreateSchema.
func (m *Manager) UpdateSchema(schema *Schema) []error {
	m.updateMu.Lock()
	defer m.updateMu.Unlock()

	exists, err := m.schemaExists(schema.Name)
	if err != nil {
		return []error{err}
	} else if !exists {
		m.log.warnf("schema '%s' does not exist, creating...", schema.Name)
		return m.createSchema(schema)
	}

	return m.convergeSchema(schema)
}

func (m *Manager) convergeSchema(schema *Schema) []error {
	// Mark existing state as locked/outdated
	err := m.lockSchema(schema.Name)
	if err != nil {
		return []error{err}
	}

	// Update builds to reflect request
	state := make([][]*BuildMetadata, 0, len(schema.Challenges))
	errs := []error{}
	for challenge, spec := range schema.Challenges {
		builds := make([]*BuildMetadata, len(spec.Seeds))
		for i, seed := range spec.Seeds {
			builds[i] = &BuildMetadata{
				Seed:          seed,
				Format:        schema.FlagFormat,
				Challenge:     challenge,
				Schema:        schema.Name,
				InstanceCount: spec.InstanceCount,
			}

			err := m.openBuild(builds[i])
			if err != nil {
				errs = append(errs, err)
				continue
			}
		}
		state = append(state, builds)
	}

	// Release obsolete builds
	err = m.cleanupSchemaResources(schema.Name)
	if err != nil {
		errs = append(errs, err)
	}

	// Create missing builds and converge instances
	for _, builds := range state {
		err := m.generateBuilds(builds)
		if err != nil {
			errs = append(errs, err)
			continue
		}

		if len(builds) == 0 {
			continue
		}

		// All builds in this group share one challenge; non-service challenges
		// (artifact-only, flag-only) are fully delivered by their builds, so
		// their instance target is zero regardless of the schema's
		// instance_count.  The teardown loop below then removes any instances
		// left over from versions of cmgr that still launched placeholders.
		cMeta, err := m.lookupChallengeMetadata(builds[0].Challenge)
		if err != nil {
			errs = append(errs, err)
			continue
		}

		for _, buildMeta := range builds {
			target := schema.Challenges[buildMeta.Challenge].InstanceCount
			if !cMeta.NeedsInstance() {
				// Checked before the on-demand/locked short-circuit so leftover
				// placeholder instances are torn down even when the schema entry
				// is DYNAMIC_INSTANCES or LOCKED.
				target = 0
			} else if target == DYNAMIC_INSTANCES || target == LOCKED {
				continue
			}

			errs = append(errs, m.convergeBuildInstances(buildMeta, cMeta, target)...)
		}
	}

	return errs
}

// convergeBuildInstances brings the number of instances of one build to
// target: surplus ones are stopped, missing ones launched through placement
// under the restart limits. It is the instance half of a schema converge, and
// what a rebuild runs for a persistent build once its restarts are done, so an
// instance the restart could not keep is relaunched by the same update that
// removed it. The caller resolves target (0 for a non-service challenge, and
// nothing to do for DYNAMIC_INSTANCES or LOCKED) and holds updateMu.
func (m *Manager) convergeBuildInstances(buildMeta *BuildMetadata, cMeta *ChallengeMetadata, target int) []error {
	errs := []error{}

	instances, err := m.getBuildInstances(buildMeta.Id)
	if err != nil {
		return append(errs, err)
	}
	m.log.debugf("converging %s/%d: %d found, need %d", buildMeta.Challenge, buildMeta.Id, len(instances), target)
	for i := target; i < len(instances); i++ {
		iMeta, err := m.lookupInstanceMetadata(instances[i])
		if err != nil {
			errs = append(errs, err)
			continue
		}

		err = m.stopInstance(iMeta)
		if err != nil {
			errs = append(errs, err)
		}
	}

	for i := len(instances); i < target; i++ {
		if len(buildMeta.Images) == 0 {
			// Lazy lookup for case where we resized
			buildMeta, err = m.lookupBuildMetadata(buildMeta.Id)
			if err != nil {
				errs = append(errs, err)
				break
			}
		}
		_, err = m.newInstance(buildMeta, nil, m.restartLimits())
		if err != nil {
			errs = append(errs, err)
			break
		}
	}

	return errs
}

// Tears down all instances and builds belonging to the schema.  Serialized
// with rebuilds and other schema operations; see CreateSchema.
func (m *Manager) DeleteSchema(name string) error {
	m.updateMu.Lock()
	defer m.updateMu.Unlock()

	err := m.lockSchema(name)
	if err != nil {
		return err
	}

	return m.cleanupSchemaResources(name)
}

func (m *Manager) cleanupSchemaResources(name string) error {
	instances, err := m.removedSchemaInstances(name)
	for _, id := range instances {
		iMeta, err := m.lookupInstanceMetadata(id)
		if err != nil {
			return err
		}

		err = m.stopInstance(iMeta)
		if err != nil {
			return err
		}
	}

	builds, err := m.removedSchemaBuilds(name)
	for _, id := range builds {
		err = m.destroyImages(id)
		if err != nil {
			return err
		}
	}

	return nil
}

// Returns the fully-nested metadata for the schema from challenges to the
// associated builds which belong to the schema through to the instances
// currently running (to include dynamic instances).
func (m *Manager) GetSchemaState(name string) ([]*ChallengeMetadata, error) {
	builds, err := m.getSchemaBuilds(name)
	if err != nil {
		return nil, err
	}

	challenges := []*ChallengeMetadata{}
	var challenge *ChallengeMetadata

	for _, buildId := range builds {
		build, err := m.lookupBuildMetadata(buildId)
		if err != nil {
			return nil, err
		}

		build.Instances, err = m.lookupBuildInstances(build.Id)
		if err != nil {
			return nil, err
		}

		if challenge != nil && challenge.Id != build.Challenge {
			challenges = append(challenges, challenge)
			challenge = nil
		}

		if challenge == nil {
			challenge, err = m.lookupChallengeMetadata(build.Challenge)
			if err != nil {
				return nil, err
			}
			challenge.Builds = []*BuildMetadata{}
		}

		challenge.Builds = append(challenge.Builds, build)
	}

	if challenge != nil {
		challenges = append(challenges, challenge)
	}

	return challenges, nil
}

func (m *Manager) GetChallengeMetadata(challenge ChallengeId) (*ChallengeMetadata, error) {
	return m.lookupChallengeMetadata(challenge)
}

func (m *Manager) GetBuildMetadata(build BuildId) (*BuildMetadata, error) {
	return m.lookupBuildMetadata(build)
}

func (m *Manager) GetInstanceMetadata(instance InstanceId) (*InstanceMetadata, error) {
	iMeta, err := m.lookupInstanceMetadata(instance)
	if err != nil {
		return nil, err
	}
	// Resolve the player-facing address at read time so a worker's public
	// address can be corrected by re-adding it, without touching instances.
	if iMeta.Worker != "" {
		iMeta.WorkerPublic = m.workerPublicAddr(iMeta.Worker)
	}
	return iMeta, nil
}

func (m *Manager) DumpState(challenges []ChallengeId) ([]*ChallengeMetadata, error) {
	allChallenges, err := m.dumpState()
	if len(challenges) == 0 {
		return allChallenges, err
	}

	chalMap := make(map[ChallengeId]*ChallengeMetadata)
	results := []*ChallengeMetadata{}
	for _, challenge := range allChallenges {
		chalMap[challenge.Id] = challenge
	}

	for _, cid := range challenges {
		meta, ok := chalMap[cid]
		if !ok {
			err = fmt.Errorf("could not find challenge '%s'", cid)
			m.log.error(err)
			return nil, err
		}
		results = append(results, meta)
	}

	return results, nil
}

// Returns a byte array with the contents of the Dockerfile associated with
// `challengeType` (if it exists).  If the challenge type does not exist, then
// an empty array is returned.
func (m *Manager) GetDockerfile(challengeType string) []byte {
	dockerfile, _ := dockerfiles.Get(challengeType)
	return dockerfile
}

func (m *Manager) checkPrune() {
	if m.pruneAge <= 0 {
		return
	}

	now := time.Now().UnixNano()
	last := m.lastPruneUnix.Load()

	// Fast path: interval hasn't elapsed — no lock needed.
	if time.Duration(now-last) < m.pruneInterval {
		return
	}

	// CAS to claim the prune slot; only one goroutine wins per interval.
	if !m.lastPruneUnix.CompareAndSwap(last, now) {
		return
	}

	go func() {
		if err := m.Prune(); err != nil {
			m.log.errorf("failed to prune old instances: %s", err)
		}
	}()
}

func (m *Manager) Prune() error {
	// Guard the exported method directly: pruneAge <= 0 means pruning is
	// disabled. Without this, a direct call would compute datetime('now', '-0
	// seconds') == now and delete essentially every on-demand instance. checkPrune
	// also short-circuits, but Prune is public and may be called on its own.
	if m.pruneAge <= 0 {
		return nil
	}

	m.log.debugf("pruning on-demand instances older than %s", m.pruneAge)

	// On-demand instances belong to builds with instancecount == DYNAMIC_INSTANCES
	// (-1). Every other instancecount is excluded: fixed-pool builds (a positive
	// count, maintained by the schema converge loop) and LOCKED builds (-2, a
	// removed/locked schema) must never be pruned here. Note this selects on
	// instancecount, not the schema name: schema-defined dynamic challenges keep
	// their real schema (e.g. "picoctf-2024"), so a name-based filter would miss
	// them entirely.
	//
	// This only clears the database rows (and, via ON DELETE CASCADE, their port
	// and container assignments). It does not stop containers: by the time this
	// fires the external cleanup (API/Celery) and docker_reaper have normally
	// already removed them, so the goal here is to reclaim DB rows and assignable
	// ports rather than to tear down live Docker resources. We also prune rows with
	// NULL created_at (legacy entries that predate the column).
	//
	// The age check is restricted to finalized instances. Unfinalized rows are
	// in-progress (or crashed) launches and are owned exclusively by the 5-minute
	// crash-GC below; without this guard a small pruneAge could delete a launch
	// that is still mid-flight. Legacy NULL-created_at rows are always finalized,
	// so pruning them unconditionally is safe.
	query := `
		DELETE FROM instances
		WHERE id IN (
			SELECT i.id
			FROM instances AS i
			JOIN builds AS b ON i.build = b.id
			WHERE b.instancecount = ?
			AND (i.created_at IS NULL OR (i.is_finalized = 1 AND i.created_at < datetime('now', ?)))
		);`

	// Render pruneAge to whole seconds for SQLite's datetime() window, rounding
	// up: truncating down would prune slightly more aggressively than configured,
	// and a sub-second pruneAge would truncate to a "-0 seconds" window matching
	// everything. Ceiling keeps the effective age >= configured and always >= 1s.
	ageSeconds := int((m.pruneAge + time.Second - time.Nanosecond) / time.Second)

	res, err := m.db.Exec(query, DYNAMIC_INSTANCES, fmt.Sprintf("-%d seconds", ageSeconds))
	if err != nil {
		return err
	}

	count, _ := res.RowsAffected()
	if count > 0 {
		m.log.infof("pruned %d old instances", count)
	}

	// Clean up unfinalized instances (crashed launches) older than 5 minutes.
	//
	// Their workers are read first, because deleting the rows is what makes
	// their leftovers findable. A launch killed mid-flight leaves containers
	// running (RestartPolicy "always"), and reconcileWorker spares them for
	// exactly as long as a row still names them: the pass at cmgrd start
	// walks straight past them. Once these rows are gone they are orphans,
	// which is the state that pass exists to clear — so run it again, for
	// just those workers, rather than leaving them to hold their published
	// ports until the next worker-add or cmgrd start.
	var gcWorkers []string
	gcWorkerQuery := `SELECT DISTINCT worker FROM instances WHERE is_finalized = 0 AND created_at < datetime('now', '-5 minutes') AND worker != '';`
	if err := m.db.Select(&gcWorkers, gcWorkerQuery); err != nil {
		m.log.errorf("failed to list the workers of unfinalized instances: %s", err)
	}

	gcQuery := `DELETE FROM instances WHERE is_finalized = 0 AND created_at < datetime('now', '-5 minutes');`
	gcRes, err := m.db.Exec(gcQuery)
	if err == nil {
		gcCount, _ := gcRes.RowsAffected()
		if gcCount > 0 {
			m.log.infof("garbage collected %d unfinalized instances", gcCount)
			m.reclaimAfterGC(gcWorkers)
		}
	} else {
		m.log.errorf("failed to garbage collect unfinalized instances: %s", err)
	}

	return nil
}
