package cmgr

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// A hand-over is how a build reaches a daemon on an external build plane
// (issue #18). PUT /challenges/{id} carries the challenge as the build plane
// scanned it and each build as the build there left it, with the images
// already pushed to the registry. The daemon verifies before it writes:
// every build's content identity is recomputed from the inputs the payload
// names, and every image tag is asked of the registry, so a row is only ever
// recorded for images the workers can pull. Then it does with the challenge
// what an update does with one the scan found -- persists it under the
// verdict classifyChallenge gives, opens or finds each build's row by its
// (schema, format, challenge, seed), and hands the build to reconcileBuild,
// which commits the generation and restarts what runs it exactly as a
// rebuild would.
//
// Nothing but JSON travels: a hand-over is metadata and image tags. The
// artifact bundles stay on the build plane that made them, where what
// publishes them to players reads them, and this daemon is told only that a
// build has them (HasArtifacts) so the platform knows to offer a download.

// ErrLocalBuildPlane refuses a hand-over on a local build plane: that daemon
// builds its own challenges from its tree, and a build handed to it would be
// rebuilt over by the next update. corkd reports it as 409.
var ErrLocalBuildPlane = errors.New("the build plane is local: this daemon builds its own challenges and takes no hand-over")

// ErrHandOverInvalid is a hand-over refused for what it says: a payload that
// contradicts itself, or a build whose content checksum is not what its
// inputs give. Nothing was written. corkd reports it as 400.
var ErrHandOverInvalid = errors.New("invalid hand-over")

// ErrNotInRegistry is a hand-over refused for what the registry says: an
// image it names is not served under its tag, so no worker could pull it.
// Nothing was written. corkd reports it as 409.
var ErrNotInRegistry = errors.New("not in the registry")

// ErrChallengeHasBuilds refuses to remove a challenge with builds on record:
// they go with the schema that wants them (DeleteSchema) or with Destroy,
// never silently with the challenge. corkd reports it as 409.
var ErrChallengeHasBuilds = errors.New("the challenge still has builds")

// HandOver is what PUT /challenges/{id} carries.
type HandOver struct {
	// Challenge as the build plane describes it, in the shape GET /state
	// gives, with Builds the builds being handed over: seed, format, schema
	// and instance count as the schema names them there, the flag and
	// lookup data, the images with their exposed ports, whether the build
	// published artifacts, and the content and source checksums the build
	// was stamped with. A build's id is adopted as delivered -- the build
	// plane's bundle is named by it and the platform addresses the artifact
	// files by the id this daemon reports, so the two numberings are one.
	// Instances, last-solved times and the rollback generation are this
	// daemon's own and are ignored.
	Challenge *ChallengeMetadata `json:"challenge"`
	// PinFingerprint is the base image pin fingerprint the builds were made
	// under, 0 with pinning off: an input to the content checksum this
	// daemon recomputes for every build, and one it does not otherwise have
	// (the pins live on the build plane).
	PinFingerprint uint32 `json:"pin_fingerprint"`
	// SeccompProfiles is the text of every seccomp profile the challenge
	// declares, keyed by the filename it declares it under. The resolved
	// text is read from the challenge directory, which this daemon does not
	// have, and it is not in the challenge's own JSON -- so without it a
	// challenge that asked for a narrower syscall set would be recorded as
	// having a profile and run under the embedded default instead.
	SeccompProfiles map[string]string `json:"seccomp_profiles,omitempty"`
}

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
func (m *Manager) HandOverChallenge(id ChallengeId, handOver *HandOver, options UpdateOptions) (*ChallengeUpdates, error) {
	if !m.externalBuildPlane {
		return nil, ErrLocalBuildPlane
	}
	if err := m.checkHandOver(id, handOver); err != nil {
		return nil, err
	}
	challenge := handOver.Challenge
	// Before the registry and before the lock: a challenge whose declared
	// seccomp policy did not arrive is refused outright rather than recorded
	// and run under the default one.
	if err := applySeccompProfiles(challenge, handOver.SeccompProfiles); err != nil {
		return nil, fmt.Errorf("%w: %s", ErrHandOverInvalid, err)
	}
	if err := m.requireInRegistry(challenge); err != nil {
		return nil, err
	}

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
		candidate, errs := m.commitHandedOverBuild(cMeta, delivered, i, revPortMap, options.PruneOldImages)
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
	// An id is a name this daemon will put in a registry URL and an image
	// tag, and on an external plane it arrives from the network rather than
	// from a tree this daemon scanned. Nothing downstream re-checks it, so
	// an id the loader could never have produced is refused here.
	if !validChallengeId(id) {
		return invalid("'%s' is not a challenge id: %s", id, challengeIdShape)
	}
	if challenge.ChallengeType == "" {
		return invalid("'%s' has no challenge type", id)
	}
	if len(challenge.Hosts) == 0 {
		return invalid("'%s' names no hosts", id)
	}
	hosts := map[string]bool{}
	for _, host := range challenge.Hosts {
		// A host name is the other half of an image tag (see dockerId) and
		// so reaches a registry URL exactly as the challenge id does. It is
		// a Dockerfile stage name on the plane that produced it, which the
		// loader matches with `\w+`, so anything else could not have been
		// built here.
		if !validHostName(host.Name) {
			return invalid("'%s' names the host %q, which is not a stage name: %s", id, host.Name, hostNameShape)
		}
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
		// A build arrives under the id its artifact bundle is named by on
		// the plane that made it, and that id is what this daemon records
		// and reports for the platform to address those files with. A
		// payload with no id would be given one drawn here, which is the
		// mismatch the adoption exists to close, so it is refused rather
		// than quietly numbered.
		if build.Id <= 0 {
			return invalid("%s carries no build id, and the id a build is handed over under is the id its artifact bundle is named by", where)
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
				return fmt.Errorf("could not ask the registry for %s: %w; %s", imageName, err, registryRecovery)
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
func (m *Manager) commitHandedOverBuild(cMeta *ChallengeMetadata, delivered *BuildMetadata, index int, revPortMap map[string]string, pruneOldImages bool) (*replacedImages, []error) {
	row := &BuildMetadata{
		// The build plane's own id, adopted rather than replaced. The bundle
		// it extracted is named by this number and an artifact server
		// publishes it under that name, while the platform can only address
		// the files by the id this daemon reports -- so the two numberings
		// have to be one. There is exactly one build plane, so its ids are
		// an id space this daemon can borrow whole.
		Id:            delivered.Id,
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
	// openBuild reads the row back, so a build already on record under a
	// different number says so here: the build plane has been rebuilt and
	// renumbered since this build was handed over. Adopting the new id would
	// mean renumbering a row that images, lookupData and instances all
	// reference ON UPDATE RESTRICT -- and every finalized build has image and
	// lookup rows, so the update is always blocked, not only when something
	// is running. It is refused instead, and refused loudly, because the
	// alternative is the orchestrator reporting an id that addresses no
	// bundle.
	if delivered.Id != 0 && row.Id != delivered.Id {
		return nil, []error{fmt.Errorf(
			"%s is on record as build %d and was handed over as build %d: the build plane has renumbered it, and its artifact bundle is now named for the new id; release the schema (remove-schema) and hand it over again",
			handOverBuildName(index, cMeta.Id, delivered), row.Id, delivered.Id)}
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

	fail := func(err error) (*replacedImages, []error) {
		m.log.error(err)
		errs := []error{err}
		if opened {
			if rmErr := m.removeBuildMetadata(row.Id); rmErr != nil {
				errs = append(errs, rmErr)
			}
		}
		return nil, errs
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

// challengeIdShape describes challengeIdRe for the operator reading a refusal.
const challengeIdShape = "an optional namespace of '/'-separated lowercase alphanumeric segments, then a name of lowercase letters, numerals and '-' that neither starts nor ends with '-'"

// challengeIdRe is exactly what the loader can produce: an optional namespace
// of lowercase alphanumeric segments (validated in loadMarkdownChallenge) and
// a name that sanitizeName has lowercased, had its non-alphanumerics turned
// into '-' and been trimmed of leading and trailing '-'. Runs of '-' are
// deliberately allowed: sanitizeName leaves one per non-alphanumeric, so "over
// the wire" becomes "over-the-wire" and "a  b" becomes "a--b".
var challengeIdRe = regexp.MustCompile(`^([a-z0-9]+(/[a-z0-9]+)*/)?[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// validChallengeId reports whether an id is one the loader could have made.
// It is the whole of the defence for the strings that reach a registry URL
// path and an image tag: a hand-over is taken from the network, and neither
// instanceImageName nor registryManifestRequest can tell a challenge id from
// the punctuation that would re-divide the request they build.
func validChallengeId(id ChallengeId) bool {
	return challengeIdRe.MatchString(string(id))
}

// hostNameShape describes validHostName for the operator reading a refusal.
const hostNameShape = "letters, numerals and '_'"

// hostNameRe is what a host name can be: a Dockerfile stage name, which the
// loader finds with `FROM +\S+(?: +[aA][sS] +(\w+))?` (see loadChallenge), so
// \w+ is the whole of it. A host name goes into an image tag beside the
// challenge id (dockerId) and from there into a registry URL path, and on an
// external build plane it arrives from the network like everything else in a
// hand-over.
var hostNameRe = regexp.MustCompile(`^\w+$`)

// validHostName reports whether a host name is one a build could have made.
func validHostName(name string) bool {
	return hostNameRe.MatchString(name)
}
