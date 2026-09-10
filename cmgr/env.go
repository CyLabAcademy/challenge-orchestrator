package cmgr

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

// legacySettingsInUse lists the CMGR_ variables present in the environment,
// sorted, whether or not a CORK_ twin overrides them: each is a line in
// someone's unit file or shell profile that has to change before the names
// go away.
func legacySettingsInUse() []string {
	var names []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, legacyEnvPrefix) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// warnLegacyEnv names every pre-rename setting found at startup, once.
func (m *Manager) warnLegacyEnv() {
	for _, name := range legacySettingsInUse() {
		m.log.warnf("%s is set; cork reads that setting as %s now, and the CMGR_ names go away two minor releases after the rename",
			name, envPrefix+strings.TrimPrefix(name, legacyEnvPrefix))
	}
}
