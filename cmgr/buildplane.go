package cmgr

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
)

// BUILD_PLANE_ENV selects where challenge images are built: 'local' (the
// default) on the docker daemon this process reaches through DOCKER_HOST,
// from the challenge tree in CMGR_DIR; or 'external', where something else
// builds, pushes to the registry and hands the finished builds to this
// daemon, which then has no docker daemon and no challenge tree of its own.
const BUILD_PLANE_ENV string = "CMGR_BUILD_PLANE"

// The two values of BUILD_PLANE_ENV, and what BuildPlane reports.
const (
	BuildPlaneLocal    string = "local"
	BuildPlaneExternal string = "external"
)

// ErrExternalBuildPlane answers every operation that needs the local daemon
// or the challenge tree when the build plane is external: an update and its
// dry run, a manual build, the base image pins, and a schema converge that
// wants a build nobody has handed over. cmgrd reports it as 409: the request
// is well-formed, this daemon is just not the one that builds.
var ErrExternalBuildPlane = errors.New("the build plane is external: this daemon does not build")

// ErrNoWorkers refuses a launch when no worker is registered. On a local
// build plane that launch runs on the local daemon; an external one has
// none. A fleet with no worker at all is a failure of the fleet, not a
// passing state like every worker being overloaded, so unlike
// ErrAllWorkersOverloaded it is not retryable: the platform surfaces it, and
// a player's retry costs less than a retry loop holding its launch workers.
var ErrNoWorkers = errors.New("no workers are registered")

// initBuildPlane reads BUILD_PLANE_ENV, an empty value counting as unset. It
// runs before anything that reads the challenge tree, the pin file or
// DOCKER_HOST, since on an external build plane none of them apply.
func (m *Manager) initBuildPlane() error {
	switch value := os.Getenv(BUILD_PLANE_ENV); value {
	case "", BuildPlaneLocal:
		m.externalBuildPlane = false
	case BuildPlaneExternal:
		m.externalBuildPlane = true
		m.log.info("build plane: external (this daemon builds nothing; every instance runs on a worker)")
	default:
		err := fmt.Errorf("%s must be '%s' or '%s', not '%s'", BUILD_PLANE_ENV, BuildPlaneLocal, BuildPlaneExternal, value)
		m.log.error(err)
		return err
	}
	return nil
}

// BuildPlane reports the configured build plane, BuildPlaneLocal or
// BuildPlaneExternal.
func (m *Manager) BuildPlane() string {
	if m.externalBuildPlane {
		return BuildPlaneExternal
	}
	return BuildPlaneLocal
}

// noteIgnoredSetting says at startup that a setting only a local build plane
// reads is present. A unit file that still carries CMGR_DIR or DOCKER_HOST
// most likely expects an update to work here, and the answer to that is a
// 409 later rather than a silent no-op; naming the setting now is the
// earlier of the two. Presence is what counts: an empty CMGR_DIR is the
// working directory on a local build plane, and an empty CMGR_BASE_PINS
// fails one, so neither is a value to pass over.
func (m *Manager) noteIgnoredSetting(name string) {
	if _, isSet := os.LookupEnv(name); isSet {
		m.log.warnf("%s is set but ignored: the build plane is external", name)
	}
}

// requireHandedOver is the external build plane's precondition for a
// schema converge: the challenge row, and a finished build for every
// (format, seed) the schema names, as a hand-over writes them. An entry
// that names no seeds wants nothing and is not checked, as a local
// converge never touches such an entry either. Every challenge that falls
// short is named, so one refusal lists all of them, and each wraps
// ErrExternalBuildPlane, which cmgrd answers with 409. Two queries answer
// for the whole schema, however many builds it names.
func (m *Manager) requireHandedOver(schema *Schema) []error {
	ids := []ChallengeId{}
	if err := m.db.Select(&ids, "SELECT id FROM challenges;"); err != nil {
		return []error{err}
	}
	known := make(map[ChallengeId]bool, len(ids))
	for _, id := range ids {
		known[id] = true
	}

	type wanted struct {
		Challenge ChallengeId `db:"challenge"`
		Seed      int         `db:"seed"`
	}
	rows := []wanted{}
	err := m.db.Select(&rows, "SELECT challenge, seed FROM builds WHERE schema = ? AND format = ? AND flag != '';",
		schema.Name, schema.FlagFormat)
	if err != nil {
		return []error{err}
	}
	built := make(map[wanted]bool, len(rows))
	for _, row := range rows {
		built[row] = true
	}

	errs := []error{}
	for _, challenge := range slices.Sorted(maps.Keys(schema.Challenges)) {
		spec := schema.Challenges[challenge]
		if len(spec.Seeds) == 0 {
			continue
		}
		if !known[challenge] {
			errs = append(errs, fmt.Errorf("challenge '%s' has not been handed over: %w", challenge, ErrExternalBuildPlane))
			continue
		}
		missing := []int{}
		for _, seed := range spec.Seeds {
			if !built[wanted{challenge, seed}] {
				missing = append(missing, seed)
			}
		}
		if len(missing) > 0 {
			errs = append(errs, missingHandOverError(challenge, schema.Name, schema.FlagFormat, missing))
		}
	}
	return errs
}

// missingHandOverError names the builds of one challenge that a schema
// wants and no hand-over has delivered. The precondition above and the
// safety net under it (requireIngestedBuilds) report the shortfall in the
// same words; each wraps ErrExternalBuildPlane, which cmgrd answers with
// 409.
func missingHandOverError(challenge ChallengeId, schema, format string, seeds []int) error {
	listed := make([]string, 0, len(seeds))
	for _, seed := range seeds {
		listed = append(listed, strconv.Itoa(seed))
	}
	return fmt.Errorf("no build of '%s' for schema '%s' (format '%s', seed %s) has been handed over: %w",
		challenge, schema, format, strings.Join(listed, ", "), ErrExternalBuildPlane)
}

// checkExternalBuildPlane refuses to start an external build plane on a
// database it could not serve, once the migrations are through. Builds
// still without a content checksum need the local daemon to migrate (their
// images are retagged and pushed under the new form), which is why
// migrateBuildChecksums leaves them alone here; and an instance placed on
// the local daemon is out of this daemon's reach (its worker column is
// empty): a stop here could only drop its records and leave whatever runs
// behind, which is what stopInstance does for one that turns up later, but
// not a state to start into knowingly. Both are named in one message, with
// the two ways out: the daemon that built them, which still has the images
// and can reach the instances, or dropping the rows.
func (m *Manager) checkExternalBuildPlane() error {
	if !m.externalBuildPlane {
		return nil
	}
	legacy, err := unmigratedBuilds(m.db)
	if err != nil {
		m.log.errorf("could not count the builds without a content checksum: %s", err)
		return err
	}
	var local int
	if err := m.db.Get(&local, "SELECT COUNT(1) FROM instances WHERE worker = '';"); err != nil {
		m.log.errorf("could not count the instances placed on the local daemon: %s", err)
		return err
	}
	if legacy == 0 && local == 0 {
		return nil
	}
	what := []string{}
	if legacy > 0 {
		what = append(what, fmt.Sprintf("%d build(s) have no content checksum (they predate content-addressed image tags, or their migration never finished)", legacy))
	}
	if local > 0 {
		what = append(what, fmt.Sprintf("%d instance(s) were placed on the local daemon", local))
	}
	err = fmt.Errorf("%s: an external build plane can neither migrate the builds nor reach the instances. Start cmgrd once with %s=%s on the host that built them (it retags and pushes the images, and can stop the instances), or remove those rows",
		strings.Join(what, ", and "), BUILD_PLANE_ENV, BuildPlaneLocal)
	m.log.error(err)
	return err
}
