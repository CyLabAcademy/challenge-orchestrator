package cmgr

import (
	"os"
	"testing"
)

// The gate matters more than the default: purging without a registry would
// remove the only copy of images that instances run.
func TestInitPurgeAfterPush(t *testing.T) {
	tests := []struct {
		name     string
		env      string
		envSet   bool
		registry string
		want     bool
	}{
		{name: "on by default in registry mode", registry: "zot.internal:5000", want: true},
		{name: "off when explicitly false", env: "false", envSet: true, registry: "zot.internal:5000", want: false},
		{name: "off when explicitly 0", env: "0", envSet: true, registry: "zot.internal:5000", want: false},
		{name: "off when explicitly off", env: "off", envSet: true, registry: "zot.internal:5000", want: false},
		{name: "on for any other value", env: "yes", envSet: true, registry: "zot.internal:5000", want: true},
		// The hard gate. Single-host cmgr never pushes, and ensureImages does
		// not pull without a registry, so the builder's copy is the only one.
		{name: "refused without a registry", registry: "", want: false},
		{name: "refused without a registry even when asked for", env: "true", envSet: true, registry: "", want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			os.Unsetenv(PURGE_AFTER_PUSH_ENV)
			if tc.envSet {
				t.Setenv(PURGE_AFTER_PUSH_ENV, tc.env)
			}
			m := &Manager{log: newLogger(DISABLED), challengeRegistry: tc.registry}
			m.initPurgeAfterPush()
			if m.purgeAfterPush != tc.want {
				t.Errorf("purgeAfterPush = %v, want %v (registry %q, env %q set=%v)",
					m.purgeAfterPush, tc.want, tc.registry, tc.env, tc.envSet)
			}
		})
	}
}

// purgeBuiltImages must be inert rather than merely harmless when it is off or
// unusable: it is called on the build path, where a stray docker round trip
// would be paid per build. A nil docker client makes any call panic, which is
// the assertion.
func TestPurgeBuiltImagesDoesNothingWhenDisabled(t *testing.T) {
	build := &BuildMetadata{
		Challenge: "cmgr/examples/custom-socat",
		Images:    []Image{{Host: "challenge"}, {Host: "builder"}},
	}

	for _, tc := range []struct {
		name     string
		purge    bool
		registry string
	}{
		{name: "flag off", purge: false, registry: "zot.internal:5000"},
		{name: "no registry", purge: true, registry: ""},
		{name: "both", purge: false, registry: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &Manager{
				log:               newLogger(DISABLED),
				challengeRegistry: tc.registry,
				purgeAfterPush:    tc.purge,
				cli:               nil, // any docker call panics
			}
			m.purgeBuiltImages(build) // must return without touching m.cli
		})
	}
}
