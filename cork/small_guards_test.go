package cork

import (
	"errors"
	"fmt"
	"testing"
)

// manifestUnknown recognizes a registry saying "no such manifest" where the
// daemon hands it back as text rather than as a not-found. It decides between
// "not pushed yet" and "the registry did not answer", and getting that wrong
// either re-pushes or records a build that is not there.
func TestManifestUnknown(t *testing.T) {
	absent := []string{
		"manifest unknown",
		"MANIFEST UNKNOWN",
		"errors: manifest unknown: manifest unknown",
		"no such manifest: registry.example.org/chal:abc123",
		"Error response from daemon: manifest not found",
	}
	for _, msg := range absent {
		if !manifestUnknown(errors.New(msg)) {
			t.Errorf("did not recognize an absent manifest: %q", msg)
		}
	}

	// A registry that could not be reached says nothing about whether the tag
	// is there, and must not be read as "absent".
	present := []string{
		"connection refused",
		"context deadline exceeded",
		"unauthorized: authentication required",
		"500 Internal Server Error",
		"manifest",
	}
	for _, msg := range present {
		if manifestUnknown(errors.New(msg)) {
			t.Errorf("read a non-answer as an absent manifest: %q", msg)
		}
	}

	// The daemon wraps these on the way out.
	wrapped := fmt.Errorf("inspecting chal:abc123: %w", errors.New("manifest unknown"))
	if !manifestUnknown(wrapped) {
		t.Error("did not recognize an absent manifest through wrapping")
	}
}

// envSlots takes a per-daemon slot count from the environment, 1 to 16, and
// falls back to the default for anything else rather than letting a typo
// wedge or flood a daemon.
func TestEnvSlots(t *testing.T) {
	const def = 2

	tests := []struct {
		value string
		want  int
	}{
		{"1", 1},   // lower bound
		{"8", 8},   // ordinary
		{"16", 16}, // upper bound
		{"0", def}, // below: an unbuffered semaphore admits nobody
		{"17", def},
		{"-1", def},
		{"", def},
		{"abc", def},
		{"2.5", def},
		{" 4", def}, // Atoi does not trim
	}

	for _, tc := range tests {
		t.Run(tc.value, func(t *testing.T) {
			m := &Manager{log: newLogger(DISABLED)}
			t.Setenv(CONCURRENT_LAUNCHES_ENV, tc.value)

			if got := m.envSlots(CONCURRENT_LAUNCHES_ENV, def); got != tc.want {
				t.Fatalf("envSlots(%q) = %d, want %d", tc.value, got, tc.want)
			}
		})
	}
}

// Unset is the default, and the pre-rename CMGR_ name is still honored so a
// deployment configured before the rename keeps its slot count.
func TestEnvSlotsUnsetAndLegacyName(t *testing.T) {
	const def = 2
	m := &Manager{log: newLogger(DISABLED)}

	if got := m.envSlots(CONCURRENT_LAUNCHES_ENV, def); got != def {
		t.Fatalf("unset: got %d, want the default %d", got, def)
	}

	t.Setenv(legacyEnvName(CONCURRENT_LAUNCHES_ENV), "6")
	if got := m.envSlots(CONCURRENT_LAUNCHES_ENV, def); got != 6 {
		t.Fatalf("legacy name: got %d, want 6", got)
	}

	// The CORK_ name wins when both are set.
	t.Setenv(CONCURRENT_LAUNCHES_ENV, "3")
	if got := m.envSlots(CONCURRENT_LAUNCHES_ENV, def); got != 3 {
		t.Fatalf("both names set: got %d, want the CORK_ value 3", got)
	}
}

// slots exists so a Manager built without initDocker (tests) cannot end up
// with an unbuffered semaphore, which would admit nobody and hang.
func TestSlotsNeverReturnsZero(t *testing.T) {
	for _, n := range []int{-1, 0} {
		if got := slots(n); got != 1 {
			t.Errorf("slots(%d) = %d, want 1", n, got)
		}
	}
	for _, n := range []int{1, 2, 16} {
		if got := slots(n); got != n {
			t.Errorf("slots(%d) = %d, want it unchanged", n, got)
		}
	}
}

// purgeBuiltImages is a no-op unless purging is on and a registry is
// configured, since it drops the only local copy of the images. The guard runs
// before anything touches the docker client, which is what lets this Manager
// have none: reaching the client panics and fails the test.
func TestPurgeBuiltImagesGuard(t *testing.T) {
	bMeta := &BuildMetadata{
		Id: BuildId(1),
		// Non-empty so the guard is what stops the sweep, not an empty list.
		Images: []Image{{Id: ImageId(1), Host: "chal", Build: BuildId(1)}},
	}

	tests := []struct {
		name     string
		purge    bool
		registry string
	}{
		{"purging off, registry set", false, "registry.example.org"},
		{"purging on, no registry", true, ""},
		{"both off", false, ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := &Manager{log: newLogger(DISABLED)}
			m.purgeAfterPush = tc.purge
			m.challengeRegistry = tc.registry

			m.purgeBuiltImages(bMeta) // must return before reaching m.cli
		})
	}
}
