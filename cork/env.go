package cork

import (
	"os"
	"sort"
	"strings"
)

// Settings are read under CORK_ names. The CMGR_ names the fork inherited
// are still honored, so a deployment configured before the rename keeps
// working: a setting is read from its CORK_ name first and, when that is
// unset, from its CMGR_ name. A daemon started with any CMGR_ name set says so
// once, at startup. The CMGR_ names go away two minor releases after the
// rename, together with the cmgrd and cmgrd-cli aliases of the binaries.
//
// A setting that never had a CMGR_ name -- CORK_DESTINATIONS, which arrived
// with the build plane -- has nothing to fall back to and is read directly.
const (
	envPrefix       = "CORK_"
	legacyEnvPrefix = "CMGR_"
)

// legacyEnvName is the pre-rename spelling of a CORK_ setting.
func legacyEnvName(name string) string {
	return legacyEnvPrefix + strings.TrimPrefix(name, envPrefix)
}

// LookupEnv is os.LookupEnv for a setting: the CORK_ name, then its CMGR_
// name. Names outside the CORK_ prefix (DOCKER_CERT_PATH, say) are looked up
// as they are.
func LookupEnv(name string) (string, bool) {
	if v, ok := os.LookupEnv(name); ok {
		return v, true
	}
	if strings.HasPrefix(name, envPrefix) {
		return os.LookupEnv(legacyEnvName(name))
	}
	return "", false
}

// Getenv is os.Getenv with LookupEnv's fallback: the empty string when neither
// name is set.
func Getenv(name string) string {
	v, _ := LookupEnv(name)
	return v
}

// settingNames is every setting this library reads. The warning below is the
// only thing that needs the list -- each read site names its own constant --
// and it exists so the warning speaks about settings rather than about
// whatever else in the environment happens to start with CMGR_.
// TestEverySettingIsListed keeps it honest.
var settingNames = []string{
	DB_ENV, DIR_ENV, ARTIFACT_DIR_ENV, REGISTRY_ENV, REGISTRY_USER_ENV,
	REGISTRY_TOKEN_ENV, REGISTRY_CERT_DIR_ENV, LOGGING_ENV, IFACE_ENV,
	PORTS_ENV, CONCURRENT_LAUNCHES_ENV, DISK_QUOTA_ENV, PRUNE_AGE_ENV,
	DB_WAL_ENV, WORKER_POLL_INTERVAL_ENV, WORKER_POLL_TIMEOUT_ENV,
	WORKER_PING_TIMEOUT_ENV, WORKER_DEEP_PROBE_ENV,
	WORKER_MAX_MISSES_ENV, WORKER_LOAD_MISSES_ENV,
	WORKER_HEALTHY_THRESHOLD_ENV, WORKER_RECOVER_BACKOFF_ENV,
	WORKER_RECOVER_BACKOFF_MAX_ENV, WORKER_EJECTION_DECAY_ENV,
	WORKER_CONTROL_TIMEOUT_ENV,
	WORKER_PULL_TIMEOUT_ENV, WORKER_LAUNCH_WAIT_ENV, BASE_PINS_ENV,
	PURGE_AFTER_PUSH_ENV, BUILD_PLANE_ENV, maxArtifactFilesEnv,
	maxArtifactBytesEnv, maxArtifactFileBytesEnv,
}

// legacySettingsInUse lists the pre-rename names of the settings present in
// the environment, sorted, whether or not a CORK_ twin overrides them: each
// is a line in someone's unit file or shell profile that has to change
// before the names go away. Only settings: a CMGR_ variable that names no
// setting of cork's -- the CMGR_ prefix a challenge container sees, or an
// operator's own -- is none of the daemon's business to comment on.
func legacySettingsInUse() []string {
	var names []string
	for _, setting := range settingNames {
		legacy := legacyEnvName(setting)
		if _, isSet := os.LookupEnv(legacy); isSet {
			names = append(names, legacy)
		}
	}
	sort.Strings(names)
	return names
}

// warnLegacyEnv names every pre-rename setting found at startup, once.
func (m *Manager) warnLegacyEnv() {
	for _, name := range legacySettingsInUse() {
		current := envPrefix + strings.TrimPrefix(name, legacyEnvPrefix)
		// CMGR_ARTIFACT_DIR is the one old name with a second reader:
		// cmgr-artifact-server publishes what a build plane writes and looks
		// that name up, so this is not a line to delete once cork stops
		// reading it.
		if current == ARTIFACT_DIR_ENV {
			m.log.warnf("%s is set; cork reads that setting as %s now, and the CMGR_ names go away two minor releases after the rename -- keep this one as well, since cmgr-artifact-server reads it",
				name, current)
			continue
		}
		m.log.warnf("%s is set; cork reads that setting as %s now, and the CMGR_ names go away two minor releases after the rename",
			name, current)
	}
}
