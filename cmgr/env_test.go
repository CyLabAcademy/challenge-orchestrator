package cmgr

import (
	"slices"
	"strings"
	"testing"
)

// The CORK_ name wins, the CMGR_ name is read when it is unset, and a name
// outside the prefix is looked up as it is.
func TestLookupEnvPrefersCorkAndFallsBackToCmgr(t *testing.T) {
	const name = "CORK_ENV_TEST_SETTING"
	legacy := legacyEnvName(name)
	if legacy != "CMGR_ENV_TEST_SETTING" {
		t.Fatalf("legacy name of %s is %s", name, legacy)
	}

	if v, ok := LookupEnv(name); ok {
		t.Fatalf("unset setting resolved to %q", v)
	}
	if Getenv(name) != "" {
		t.Fatalf("Getenv of an unset setting is %q", Getenv(name))
	}

	t.Setenv(legacy, "old")
	if v, ok := LookupEnv(name); !ok || v != "old" {
		t.Fatalf("with only %s set, LookupEnv(%s) = %q, %v", legacy, name, v, ok)
	}
	if Getenv(name) != "old" {
		t.Fatalf("Getenv(%s) = %q; expected the CMGR_ value", name, Getenv(name))
	}

	t.Setenv(name, "new")
	if v, _ := LookupEnv(name); v != "new" {
		t.Fatalf("with both set, LookupEnv(%s) = %q; the CORK_ name must win", name, v)
	}

	// An empty CORK_ value is a value, not an absence: it must not fall
	// through to the CMGR_ one.
	t.Setenv(name, "")
	if v, ok := LookupEnv(name); !ok || v != "" {
		t.Fatalf("empty %s fell through to %q, %v", name, v, ok)
	}

	t.Setenv("CMGR_ENV_TEST_OTHER", "x")
	if v, ok := LookupEnv("ENV_TEST_OTHER"); ok {
		t.Fatalf("a name outside the CORK_ prefix resolved through CMGR_: %q", v)
	}
}

// Every setting the daemon reads is a CORK_ name, so the fallback covers it.
func TestSettingsUseTheCorkPrefix(t *testing.T) {
	for _, name := range []string{
		DB_ENV, DIR_ENV, ARTIFACT_DIR_ENV, REGISTRY_ENV, REGISTRY_USER_ENV,
		REGISTRY_TOKEN_ENV, REGISTRY_CERT_DIR_ENV, LOGGING_ENV, IFACE_ENV,
		PORTS_ENV, DISK_QUOTA_ENV, PRUNE_AGE_ENV, DB_WAL_ENV,
		CONCURRENT_LAUNCHES_ENV, WORKER_POLL_INTERVAL_ENV,
		WORKER_POLL_TIMEOUT_ENV, WORKER_MAX_MISSES_ENV,
		WORKER_CONTROL_TIMEOUT_ENV, WORKER_PULL_TIMEOUT_ENV,
		WORKER_LAUNCH_WAIT_ENV, BASE_PINS_ENV, PURGE_AFTER_PUSH_ENV,
		maxArtifactFilesEnv, maxArtifactBytesEnv, maxArtifactFileBytesEnv,
	} {
		if !strings.HasPrefix(name, envPrefix) || len(name) == len(envPrefix) {
			t.Errorf("setting %q is not a %s name", name, envPrefix)
		}
	}
}

func TestLegacySettingsInUse(t *testing.T) {
	t.Setenv("CMGR_ENV_TEST_B", "1")
	t.Setenv("CMGR_ENV_TEST_A", "")
	t.Setenv("CORK_ENV_TEST_C", "1")
	got := legacySettingsInUse()
	ia, ib := slices.Index(got, "CMGR_ENV_TEST_A"), slices.Index(got, "CMGR_ENV_TEST_B")
	if ia < 0 || ib < 0 || ia > ib {
		t.Fatalf("legacy settings %v: expected CMGR_ENV_TEST_A before CMGR_ENV_TEST_B", got)
	}
	if slices.Contains(got, "CORK_ENV_TEST_C") {
		t.Fatalf("a CORK_ name was listed as legacy: %v", got)
	}
}
