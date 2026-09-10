package cmgr

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// A hand-over is how a build reaches a daemon on an external build plane
// (issue #18). PUT /challenges/{id} carries the challenge as the build plane
// scanned it and each build as the build there left it, with the images
// already pushed to the registry and the artifact archive in the request.
// The daemon verifies before it writes: every build's content identity is
// recomputed from the inputs the payload names, and every image tag is asked
// of the registry, so a row is only ever recorded for images the workers can
// pull. Then it does with the challenge what an update does with one the
// scan found -- persists it under the verdict classifyChallenge gives, opens
// or finds each build's row by its (schema, format, challenge, seed), stages
// the archive through cacheArtifacts under that row's id, and hands the
// build to reconcileBuild, which commits the generation and restarts what
// runs it exactly as a rebuild would.

// ErrLocalBuildPlane refuses a hand-over on a local build plane: that daemon
// builds its own challenges from its tree, and a build handed to it would be
// rebuilt over by the next update. cmgrd reports it as 409.
var ErrLocalBuildPlane = errors.New("the build plane is local: this daemon builds its own challenges and takes no hand-over")

// ErrHandOverInvalid is a hand-over refused for what it says: a payload that
// contradicts itself, or a build whose content checksum is not what its
// inputs give. Nothing was written. cmgrd reports it as 400.
var ErrHandOverInvalid = errors.New("invalid hand-over")

// ErrNotInRegistry is a hand-over refused for what the registry says: an
// image it names is not served under its tag, so no worker could pull it.
// Nothing was written. cmgrd reports it as 409.
var ErrNotInRegistry = errors.New("not in the registry")

// ErrChallengeHasBuilds refuses to remove a challenge with builds on record:
// they go with the schema that wants them (DeleteSchema) or with Destroy,
// never silently with the challenge. cmgrd reports it as 409.
var ErrChallengeHasBuilds = errors.New("the challenge still has builds")

// HandOver is what PUT /challenges/{id} carries.
type HandOver struct {
	// Challenge as the build plane describes it, in the shape GET /state
	// gives, with Builds the builds being handed over: seed, format, schema
	// and instance count as the schema names them there, the flag and
	// lookup data, the images with their exposed ports, whether an archive
	// comes with it, and the content and source checksums the build was
	// stamped with. Ids, instances, last-solved times and the rollback
	// generation are this daemon's own and are ignored.
	Challenge *ChallengeMetadata `json:"challenge"`
	// PinFingerprint is the base image pin fingerprint the builds were made
	// under, 0 with pinning off: an input to the content checksum this
	// daemon recomputes for every build, and one it does not otherwise have
	// (the pins live on the build plane).
	PinFingerprint uint32 `json:"pin_fingerprint"`
}

// ArchiveSource yields the artifact archive of the build at index i of the
// hand-over's Builds, a gzip tar as GET /builds/{id}/artifacts.tar.gz serves
// one. It is asked once for every build that declares HasArtifacts, in index
// order, once the hand-over has been verified and before any of it is
// recorded (stageHandOverArchives). What it returns is closed here.
type ArchiveSource func(i int) (io.ReadCloser, error)

// HandOverChallenge records a challenge and the builds handed over with it,
// verifying first and writing only then. The verdict comes back the way an
// update reports one: the challenge in exactly one of Added, Updated,
// Refreshed, Stale and Unmodified, as recorded, its builds with their ids;
// and in Errors whatever could not be committed once the writes had begun,
// per build, as a rebuild reports its failures. An error instead is a
// refusal that wrote nothing: ErrLocalBuildPlane, ErrHandOverInvalid or
// ErrNotInRegistry, or the registry or the database not answering.
//
// A build whose row already serves the generation delivered is left as it
// is, instances included, so handing the same thing over twice changes
// nothing -- the guarantee an update gives an unmodified challenge.
func (m *Manager) HandOverChallenge(id ChallengeId, handOver *HandOver, archives ArchiveSource, options UpdateOptions) (*ChallengeUpdates, error) {
	if !m.externalBuildPlane {
		return nil, ErrLocalBuildPlane
	}
	if err := m.checkHandOver(id, handOver); err != nil {
		return nil, err
	}
	challenge := handOver.Challenge
	if err := m.requireInRegistry(challenge); err != nil {
		return nil, err
	}

	// The archives are taken here, before the lock: they are the one part
	// of a hand-over that arrives over the network, and reading them under
	// updateMu would let a client that stalls mid-body hold every update
	// and schema operation behind it for as long as it liked.
	staged, err := m.stageHandOverArchives(challenge, archives)
	if err != nil {
		return nil, err
	}
	defer func() {
		// Whatever no build took: the archive of one that failed before it
		// was installed. An installed one no longer answers to its staged
		// name, so this is then the no-op it should be.
		for _, path := range staged {
			os.Remove(path)
		}
		m.releaseStagedArchives(staged)
	}()

	// Serialized with updates, schema operations and other hand-overs: a
	// hand-over restarts and relaunches the instances of what it replaces,
	// as a rebuild does, and would work over any of them.
	m.updateMu.Lock()
	defer m.updateMu.Unlock()

	current, err := m.lookupChallengeMetadata(id)
	if err != nil {
		var unknown *UnknownIdentifierError
		if !errors.As(err, &unknown) {
			return nil, err
		}
		current = nil
	}
	stale := false
	if current != nil {
		if stale, err = m.challengeHasStaleBuild(id); err != nil {
			return nil, err
		}
	}

	// The metadata delivered is persisted before its builds, as an update
	// persists the tree's before it rebuilds (updateChallenges), so the
	// restarts below read the challenge the builds were made from. A
	// verdict that changes nothing persists nothing.
	cu := new(ChallengeUpdates)
	var bucket *[]*ChallengeMetadata
	verdict := m.classifyChallenge(current, challenge, stale)
	switch verdict {
	case verdictAdded:
		if errs := m.addChallenges([]*ChallengeMetadata{challenge}); len(errs) > 0 {
			return nil, errors.Join(errs...)
		}
		bucket = &cu.Added
	case verdictUpdated, verdictRefreshed:
		if errs := m.updateChallenges([]*ChallengeMetadata{challenge}, nil, false); len(errs) > 0 {
			return nil, errors.Join(errs...)
		}
		bucket = &cu.Updated
		if verdict == verdictRefreshed {
			bucket = &cu.Refreshed
		}
	case verdictStale:
		bucket = &cu.Stale
	default:
		bucket = &cu.Unmodified
	}
	*bucket = []*ChallengeMetadata{challenge}

	// From here on the verdict stands and failures are the builds' own.
	cMeta, err := m.lookupChallengeMetadata(id)
	if err != nil {
		cu.Errors = append(cu.Errors, err)
		return cu, nil
	}
	revPortMap, err := m.getReversePortMap(id)
	if err != nil {
		cu.Errors = append(cu.Errors, err)
		return cu, nil
	}
	replaced := []replacedImages{}
	for i, delivered := range challenge.Builds {
		candidate, errs := m.commitHandedOverBuild(cMeta, delivered, i, staged, revPortMap, options.PruneOldImages)
		cu.Errors = append(cu.Errors, errs...)
		if candidate != nil {
			replaced = append(replaced, *candidate)
		}
	}
	// As after a rebuild: pruning waits until every build is through, since
	// a displaced generation may still be another row's current or rollback
	// one (see rebuildBuilds).
	m.pruneReplacedImages(replaced)

	// What this hand-over did not bring: builds of the challenge still at an
	// earlier source generation, whose rebuilds are the build plane's to
	// hand over. Said, as a converge says it, not failed.
	if leftovers, err := m.staleBuildIds(cMeta); err != nil {
		m.log.warnf("could not check whether every build of '%s' is current: %s", id, err)
	} else if len(leftovers) > 0 {
		m.log.warnf("%d build(s) of '%s' still serve an earlier source generation and were not handed over: %v", len(leftovers), id, leftovers)
	}

	// The challenge as recorded, ids included.
	recorded, err := m.dumpChallenge(id)
	if err != nil {
		cu.Errors = append(cu.Errors, err)
	} else {
		*bucket = []*ChallengeMetadata{recorded}
	}
	return cu, nil
}

// stageHandOverArchives takes the archives a hand-over carries off the
// request and through cacheArtifacts (which is where a malformed or
// oversized one is caught) into staging files beside the ones the daemon
// serves, keyed by the index of the build each belongs to.
//
// All of them in one pass, before any row is written, for two reasons. An
// ArchiveSource reads the parts of one request in order, so a build skipped
// after a failure would leave every later one reading its neighbour's
// archive; and a hand-over refused here has written nothing, which a
// per-build failure could not promise once the rows had begun to move. The
// cost is a challenge's archives on disk at once, beside the ones already
// installed, rather than one at a time: a challenge has few seeds, and the
// alternative is reading a request body under the lock.
//
// An archive that cannot be taken refuses the hand-over as invalid,
// whether the payload is wrong or the body simply stopped arriving: the
// two are the same truncated archive from here, and the builder is the one
// that saw its own connection go.
func (m *Manager) stageHandOverArchives(challenge *ChallengeMetadata, archives ArchiveSource) (map[int]string, error) {
	m.sweepStagedArchives()
	staged := map[int]string{}
	fail := func(err error) (map[int]string, error) {
		for _, path := range staged {
			os.Remove(path)
		}
		m.releaseStagedArchives(staged)
		return nil, err
	}
	for i, build := range challenge.Builds {
		if !build.HasArtifacts {
			continue
		}
		where := handOverBuildName(i, challenge.Id, build)
		source, err := archives(i)
		if err != nil {
			return fail(fmt.Errorf("%w: %s: %s", ErrHandOverInvalid, where, err))
		}
		// A name nothing else can take -- two hand-overs stage outside the
		// lock and could otherwise be staging the same build at once --
		// which cacheArtifacts renames its own temporary file over once
		// the archive has turned out sound.
		placeholder, err := os.CreateTemp(m.artifactsDir, stagedArchivePrefix+"*.staged")
		if err != nil {
			source.Close()
			return fail(fmt.Errorf("could not stage the archive of %s: %w", where, err))
		}
		path := placeholder.Name()
		placeholder.Close()
		staged[i] = path
		// Held until this hand-over is done with it. Staging is over in a
		// moment, but the hand-over then waits its turn at updateMu and
		// commits every earlier build before this one is installed, and a
		// sweep that took it meanwhile would fail the hand-over on a file it
		// had already taken whole.
		m.holdStagedArchive(path)

		files, err := m.cacheArtifacts(source, path)
		source.Close()
		if err != nil {
			return fail(fmt.Errorf("%w: the archive of %s: %s", ErrHandOverInvalid, where, err))
		}
		if len(files) == 0 {
			return fail(fmt.Errorf("%w: %s declares artifacts but its archive holds no files", ErrHandOverInvalid, where))
		}
	}
	return staged, nil
}

const (
	// stagedArchivePrefix names the staging files of a hand-over, and is
	// what tells them apart from the archives the daemon serves (which are
	// "<build id>.tar.gz") when one has to be reclaimed.
	stagedArchivePrefix = ".cork-handover-"
	// stagedArchiveMaxAge is how long a staging file that no hand-over here
	// holds is left before a later one reclaims it. Only a dead process's
	// can be that, so this is not a race to win but a margin: long enough
	// that a clock skew or a filesystem with a coarse timestamp cannot make
	// a file look abandoned, short enough that what a crash leaves does not
	// sit there for a season.
	stagedArchiveMaxAge = time.Hour
)

// sweepStagedArchives removes the staging files of a hand-over that never
// returned -- the process died between taking an archive and installing it
// -- which nothing else would reclaim, each carrying a name no later
// hand-over reuses. What a hand-over of this process still holds is never
// swept, however long it has been waiting, so age alone only ever reclaims
// what an earlier process left. Best-effort throughout: a file that cannot
// be swept costs disk, and taking a hand-over down over it would cost more.
func (m *Manager) sweepStagedArchives() {
	entries, err := os.ReadDir(m.artifactsDir)
	if err != nil {
		m.log.warnf("could not sweep staged artifact archives: %s", err)
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), stagedArchivePrefix) {
			continue
		}
		path := filepath.Join(m.artifactsDir, entry.Name())
		// A hand-over of this process still has it: it may have been staged
		// long ago and be waiting its turn at updateMu behind a long
		// converge, and it is not abandoned however old it looks.
		if m.stagedArchiveHeld(path) {
			continue
		}
		info, err := entry.Info()
		if err != nil || time.Since(info.ModTime()) < stagedArchiveMaxAge {
			continue
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			m.log.warnf("could not remove the staged artifact archive %s: %s", path, err)
			continue
		}
		m.log.infof("removed %s, staged by a hand-over that did not finish", path)
	}
}

// holdStagedArchive marks a staging file as one a hand-over of this process
// still has in flight, and releaseStagedArchives lets a whole hand-over's set
// go. What is held is never swept (sweepStagedArchives), which is what makes
// the sweep's age threshold a margin over a dead process rather than a
// deadline this one has to beat.
func (m *Manager) holdStagedArchive(path string) {
	m.stagedArchivesMu.Lock()
	defer m.stagedArchivesMu.Unlock()
	if m.stagedArchives == nil {
		m.stagedArchives = map[string]bool{}
	}
	m.stagedArchives[path] = true
}

func (m *Manager) releaseStagedArchives(staged map[int]string) {
	m.stagedArchivesMu.Lock()
	defer m.stagedArchivesMu.Unlock()
	for _, path := range staged {
		delete(m.stagedArchives, path)
	}
}

func (m *Manager) stagedArchiveHeld(path string) bool {
	m.stagedArchivesMu.Lock()
	defer m.stagedArchivesMu.Unlock()
	return m.stagedArchives[path]
}

// handOverBuildName names one build of a hand-over as every error about it
// reports it: by its place in the payload, and by the tuple its row is
// keyed by.
func handOverBuildName(index int, challenge ChallengeId, build *BuildMetadata) string {
	return fmt.Sprintf("build %d of '%s' (schema '%s', format '%s', seed %d)",
		index, challenge, build.Schema, build.Format, build.Seed)
}

// checkHandOver is what can be verified of a hand-over without asking
// anything else: that the payload describes the challenge it is addressed
// to, that each build is of that challenge's source generation and named
// once, and that each carries the content checksum its inputs give under
// this daemon's templates -- the identity every image tag is derived from,
// so a build that fails it names images that cannot be its own.
func (m *Manager) checkHandOver(id ChallengeId, handOver *HandOver) error {
	invalid := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrHandOverInvalid, fmt.Sprintf(format, args...))
	}
	if handOver == nil {
		return invalid("no hand-over")
	}
	challenge := handOver.Challenge
	if challenge == nil {
		return invalid("no challenge")
	}
	if challenge.Id != id {
		return invalid("the payload describes '%s', the request '%s'", challenge.Id, id)
	}
	if challenge.ChallengeType == "" {
		return invalid("'%s' has no challenge type", id)
	}
	if len(challenge.Hosts) == 0 {
		return invalid("'%s' names no hosts", id)
	}
	hosts := map[string]bool{}
	for _, host := range challenge.Hosts {
		hosts[host.Name] = true
	}
	for name, port := range challenge.PortMap {
		if !hosts[port.Host] {
			return invalid("port '%s' of '%s' is on host '%s', which the challenge does not name", name, id, port.Host)
		}
	}

	template := m.templateChecksum(challenge.ChallengeType)
	seen := map[string]bool{}
	for i, build := range challenge.Builds {
		// JSON null in the builds array, which everything below would
		// dereference: this endpoint takes whatever is sent to it.
		if build == nil {
			return invalid("build %d of '%s' is null", i, id)
		}
		where := handOverBuildName(i, id, build)
		if build.Challenge != "" && build.Challenge != id {
			return invalid("%s belongs to '%s'", where, build.Challenge)
		}
		if build.Schema == "" || build.Format == "" {
			return invalid("%s names no schema or no flag format", where)
		}
		key := build.Schema + "\x00" + build.Format + "\x00" + strconv.Itoa(build.Seed)
		if seen[key] {
			return invalid("%s is handed over twice", where)
		}
		seen[key] = true
		if build.Flag == "" {
			return invalid("%s has no flag", where)
		}
		if build.InstanceCount < DYNAMIC_INSTANCES {
			return invalid("%s has instance count %d, which no schema gives", where, build.InstanceCount)
		}
		if build.SourceChecksum != challenge.SourceChecksum {
			return invalid("%s was built from source generation %x; the challenge handed over is generation %x", where, build.SourceChecksum, challenge.SourceChecksum)
		}
		want := contentChecksum(build.SourceChecksum, build.Format, handOver.PinFingerprint, template)
		if build.Checksum != want {
			return invalid("%s carries content checksum %x; its inputs (source generation %x, the flag format, pin fingerprint %x, the '%s' template) give %x",
				where, build.Checksum, build.SourceChecksum, handOver.PinFingerprint, challenge.ChallengeType, want)
		}
		if len(build.Images) == 0 {
			return invalid("%s has no images", where)
		}
		imaged := map[string]bool{}
		for _, image := range build.Images {
			if !hosts[image.Host] {
				return invalid("%s has an image for host '%s', which the challenge does not name", where, image.Host)
			}
			if imaged[image.Host] {
				return invalid("%s has two images for host '%s'", where, image.Host)
			}
			imaged[image.Host] = true
		}
		// And every host has one. The local build path makes exactly one
		// image per host by construction (executeBuild walks cMeta.Hosts), so
		// it never has to ask; here the images arrive over the wire. A build
		// short of one is not caught further down either -- requireInRegistry
		// only asks after the images it was given -- and startContainers
		// iterates the images rather than the hosts, so the launch would
		// quietly start the hosts it has and report the instance finalized.
		for _, host := range challenge.Hosts {
			if !imaged[host.Name] {
				return invalid("%s has no image for host '%s', which the challenge names", where, host.Name)
			}
		}
	}
	return nil
}

// requireInRegistry asks the registry for every image tag the builds handed
// over resolve to, the build plane's own builder image excepted (it is never
// pushed; see publishImages), and names every one it does not serve.
func (m *Manager) requireInRegistry(challenge *ChallengeMetadata) error {
	missing := []string{}
	for _, build := range challenge.Builds {
		for _, image := range build.Images {
			if image.Host == "builder" {
				continue
			}
			imageName := m.instanceImageName(challenge.Id, build, image)
			present, err := m.registryTagPresent(imageName)
			if err != nil {
				return fmt.Errorf("could not ask the registry for %s: %w", imageName, err)
			}
			if !present {
				missing = append(missing, imageName)
			}
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%s: %w", strings.Join(missing, ", "), ErrNotInRegistry)
	}
	return nil
}

// commitHandedOverBuild opens or finds the row of one build handed over,
// installs the archive staged for it, and hands the build to
// reconcileBuild, which commits the generation and brings what runs it onto
// it. A row that already serves exactly this generation is left alone
// instead: the tags are the same and so are the bytes, so there is nothing
// to commit and nothing to restart, and only the instance count -- the
// schema's, which openBuild has just written -- is converged to. A build
// that cannot be committed is dropped when the row was opened for it, or
// left at its previous generation when it already had one, exactly as a
// build whose rebuild failed (see rebuildBuilds), and reported.
func (m *Manager) commitHandedOverBuild(cMeta *ChallengeMetadata, delivered *BuildMetadata, index int, staged map[int]string, revPortMap map[string]string, pruneOldImages bool) (*replacedImages, []error) {
	row := &BuildMetadata{
		Seed:          delivered.Seed,
		Format:        delivered.Format,
		Challenge:     cMeta.Id,
		Schema:        delivered.Schema,
		InstanceCount: delivered.InstanceCount,
		// The identity it will be finalized with, so a row opened here counts
		// as a reference to its images from the start (see openBuild).
		Checksum: delivered.Checksum,
	}
	if err := m.openBuild(row); err != nil {
		return nil, []error{err}
	}
	// A row with no generation on it -- opened now, or by a hand-over that
	// did not finish -- is dropped if this one does not finish either.
	opened := row.Flag == ""
	// The row already serves what is being handed over. Every column here
	// is a function of the content the checksum names, so a row agreeing on
	// the checksum and disagreeing on one of the others is one to take
	// again rather than skip.
	held := row.Flag == delivered.Flag &&
		row.Checksum == delivered.Checksum &&
		row.SourceChecksum == delivered.SourceChecksum &&
		row.HasArtifacts == delivered.HasArtifacts
	where := handOverBuildName(index, cMeta.Id, delivered)

	build := *delivered
	build.Id = row.Id
	build.Challenge = cMeta.Id
	build.Instances = nil
	build.PrevChecksum = row.PrevChecksum
	archive := filepath.Join(m.artifactsDir, build.getArtifactsFilename())

	fail := func(err error) (*replacedImages, []error) {
		m.log.error(err)
		errs := []error{err}
		if opened {
			os.Remove(archive)
			if rmErr := m.removeBuildMetadata(row.Id); rmErr != nil {
				errs = append(errs, rmErr)
			}
		}
		return nil, errs
	}

	if path, ok := staged[index]; ok {
		// Promoted whole, as a build's own archive is (executeBuild): a
		// download finds the old archive or the new, never a partial one.
		// Done for a row that already holds the generation as well, since
		// the bytes are the same either way and a file gone missing under
		// a row still serving it is then repaired rather than left to 500.
		if err := os.Rename(path, archive); err != nil {
			return fail(fmt.Errorf("could not install the archive of %s: %w", where, err))
		}
		if directory, err := os.Open(m.artifactsDir); err == nil {
			_ = directory.Sync()
			_ = directory.Close()
		}
	}

	if held {
		m.log.infof("%s had already been handed over: its generation stands, and only what runs it is converged", where)
		// What runs a build can still be wrong where the build is not: the
		// schema may ask for a different count, which openBuild has just
		// taken from the payload, and a challenge that needs no instances
		// at all may carry placeholder rows an older cmgr launched. A
		// converge resolves both, and resolves them here the way
		// convergeSchema does -- the non-service case first, so those
		// placeholders are torn down whatever the count says. An on-demand
		// build has no count to converge to; LOCKED never reaches this,
		// since checkHandOver refuses a hand-over that names one.
		target := build.InstanceCount
		if !cMeta.NeedsInstance() {
			target = 0
		} else if target == DYNAMIC_INSTANCES {
			return nil, nil
		}
		return nil, m.convergeBuildInstances(&build, cMeta, target)
	}

	candidate, errs := m.reconcileBuild(&build, cMeta, revPortMap, pruneOldImages)
	if opened && len(errs) > 0 {
		// reconcileBuild reports a restart it could not make as well as a
		// row it could not finalize; only the second leaves a row with no
		// generation behind, and that row goes.
		if stored, err := m.lookupBuildMetadata(row.Id); err == nil && stored.Flag == "" {
			return fail(fmt.Errorf("%s was not committed: %w", where, errors.Join(errs...)))
		}
	}
	return candidate, errs
}

// RemoveChallenge takes a challenge off the catalogue: on an external build
// plane the counterpart of the hand-over that put it there, for a challenge
// the build plane no longer has. It has to have no builds on record: those
// go with the schema that wants them (DeleteSchema) or with Destroy, never
// silently with the challenge. A local build plane accepts it too, though
// there the tree decides: the next update finds the challenge again unless
// it is gone from the tree as well.
func (m *Manager) RemoveChallenge(id ChallengeId) error {
	m.updateMu.Lock()
	defer m.updateMu.Unlock()

	meta, err := m.lookupChallengeMetadata(id)
	if err != nil {
		return err
	}
	var builds int
	if err := m.db.Get(&builds, "SELECT COUNT(1) FROM builds WHERE challenge = ?;", id); err != nil {
		return err
	}
	if builds > 0 {
		return fmt.Errorf("challenge '%s' has %d build(s) on record: %w", id, builds, ErrChallengeHasBuilds)
	}
	return m.removeChallenges([]*ChallengeMetadata{meta})
}
