package cmgr

import (
	"os"
	"path/filepath"
	"regexp"
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
	for _, name := range settingNames {
		if !strings.HasPrefix(name, envPrefix) || len(name) == len(envPrefix) {
			t.Errorf("setting %q is not a %s name", name, envPrefix)
		}
	}
}

// settingNames is what the startup warning speaks about, so a setting added
// later and left out of it would go unmentioned on a box still configured
// under the old names. Read the package's own sources for CORK_ literals
// rather than trusting the list to have been updated by hand.
func TestEverySettingIsListed(t *testing.T) {
	listed := map[string]bool{}
	for _, name := range settingNames {
		listed[name] = true
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	literal := regexp.MustCompile(`"(CORK_[A-Z0-9_]+)"`)
	for _, file := range files {
		// env.go declares the prefix itself; the tests invent settings.
		if file == "env.go" || strings.HasSuffix(file, "_test.go") {
			continue
		}
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range literal.FindAllStringSubmatch(string(src), -1) {
			if !listed[m[1]] {
				t.Errorf("%s declares %s, which settingNames (env.go) does not list: a box still setting %s would not be told",
					file, m[1], legacyEnvName(m[1]))
			}
		}
	}
}

// The settings a build plane reports as ignored are settings too, and
// noteIgnoredSetting goes through the fallback to find them: a unit file
// grown from an orchestrator's carries the CMGR_ spellings, which are
// exactly the lines the warning exists to point at.
func TestIgnoredSettingsUseTheCorkPrefix(t *testing.T) {
	for _, name := range orchestratorOnlySettings {
		if !strings.HasPrefix(name, envPrefix) {
			t.Errorf("ignored setting %q is not a %s name", name, envPrefix)
		}
	}
}

func TestLegacySettingsInUse(t *testing.T) {
	unsetenv(t, DIR_ENV)
	unsetenv(t, DB_ENV)
	t.Setenv(legacyEnvName(DIR_ENV), "1")
	t.Setenv(legacyEnvName(DB_ENV), "") // empty is still a line in a unit file
	t.Setenv(REGISTRY_ENV, "1")         // the current name is not a legacy one
	// Neither a setting of cork's: the prefix a challenge container sees, and
	// an operator's own variable. Warning about either would be wrong.
	t.Setenv("CMGR_USER_ID", "1")
	t.Setenv("CMGR_SOMETHING_ELSE", "1")

	got := legacySettingsInUse()
	want := []string{legacyEnvName(DB_ENV), legacyEnvName(DIR_ENV)} // sorted
	if !slices.Equal(got, want) {
		t.Fatalf("legacy settings in use: %v, want %v", got, want)
	}
}
