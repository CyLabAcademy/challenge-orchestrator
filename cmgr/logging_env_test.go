package cmgr

import (
	"strings"
	"testing"
)

// CMGR_LOGGING is the only way to change an orchestrator's log level --
// cmgrd has no flag for it -- so it is worth testing that it is read at all:
// for a while the constant was declared and used nowhere, and cmgrd
// hardcoded INFO while its help documented this.
func TestLogLevelFromEnv(t *testing.T) {
	unsetenv(t, LOGGING_ENV)
	if level, err := LogLevelFromEnv(WARN); level != WARN || err != nil {
		t.Errorf("unset: %s, %v; want the fallback", level, err)
	}
	// Empty is unset: a unit file that exports it blank has said nothing.
	t.Setenv(LOGGING_ENV, "")
	if level, err := LogLevelFromEnv(WARN); level != WARN || err != nil {
		t.Errorf("empty: %s, %v; want the fallback", level, err)
	}

	for value, want := range map[string]LogLevel{
		"debug": DEBUG, "info": INFO, "warn": WARN, "warning": WARN,
		"error": ERROR, "disabled": DISABLED, "off": DISABLED, "none": DISABLED,
		"DEBUG": DEBUG, "Info": INFO, // however it is spelled in a unit file
	} {
		t.Setenv(LOGGING_ENV, value)
		if level, err := LogLevelFromEnv(INFO); level != want || err != nil {
			t.Errorf("%q read as %s (%v), want %s", value, level, err, want)
		}
	}

	// A typo is complained about and not fatal: refusing to start a daemon
	// over a log level would be the worse failure by a distance.
	t.Setenv(LOGGING_ENV, "verbose")
	level, err := LogLevelFromEnv(INFO)
	if level != INFO {
		t.Errorf("an unreadable level gave %s rather than the fallback", level)
	}
	if err == nil {
		t.Fatal("an unreadable level was accepted silently")
	}
	for _, want := range []string{LOGGING_ENV, "verbose", "info"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the complaint does not say %q: %s", want, err)
		}
	}
}
