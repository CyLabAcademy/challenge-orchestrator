package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeDestinations(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "destinations.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The mapping is read when it is read, not when a hand-over fails: an
// address no request could be made against is refused here, as --server is.
func TestLoadDestinations(t *testing.T) {
	d, err := loadDestinations(writeDestinations(t, "library: https://library.example.net:4200\nevent: http://event.example.net:4200/\n"))
	if err != nil {
		t.Fatalf("loadDestinations: %s", err)
	}
	if len(d.byName) != 2 {
		t.Fatalf("read %d destination(s): %v", len(d.byName), d.byName)
	}
	// The trailing slash is trimmed, as it is for --server: everything this
	// is joined with starts with one.
	if d.byName["event"] != "http://event.example.net:4200" {
		t.Errorf("event resolved to %q", d.byName["event"])
	}
	// No default with more than one: there is nothing to default to.
	if d.defName != "" {
		t.Errorf("two destinations left a default of %q", d.defName)
	}

	// Absent is not an error: a build plane given --server configures none.
	// One given neither is refused by resolve, when it comes to route.
	if d, err := loadDestinations(filepath.Join(t.TempDir(), "absent.yaml")); err != nil || len(d.byName) != 0 {
		t.Errorf("an absent destinations file: %v, %v", d, err)
	}
	if d, err := loadDestinations(""); err != nil || len(d.byName) != 0 {
		t.Errorf("no destinations file configured: %v, %v", d, err)
	}

	for _, bad := range []struct{ content, says string }{
		{"library: not-an-address\n", "not an http(s) address"},
		{"library: http://\n", "names no host"},
		{"library:\n", "has no address"},
		{"library: [a, b]\n", "reading"},     // an alias for two is not a mapping
		{"\"\": http://x:4200\n", "no name"}, // yaml allows an empty key; this does not
		// Two names for one orchestrator: whereSchemaLives would find every
		// schema there under both and report it served twice, which is the
		// state migrate-schema refuses to act on -- so nothing bound to it
		// could be moved again. The second is the same address trailing
		// slash and all, since that is trimmed before this is asked.
		{"event: http://one:4200\nlibrary: http://one:4200/\n", "are both"},
	} {
		if _, err := loadDestinations(writeDestinations(t, bad.content)); err == nil {
			t.Errorf("%q was accepted", strings.TrimSpace(bad.content))
		} else if !strings.Contains(err.Error(), bad.says) {
			t.Errorf("%q was refused with %q, which does not say %q", strings.TrimSpace(bad.content), err, bad.says)
		}
	}
}

// A schema that names no destination means the only one there is, and is
// refused once there is more than one. The rule changes exactly when the
// risk appears: nothing to be ambiguous about with one configured, and a
// real question with two.
func TestResolveDestination(t *testing.T) {
	one, err := loadDestinations(writeDestinations(t, "library: http://library:4200\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := one.resolve("quiet-year", ""); err != nil || got != "http://library:4200" {
		t.Errorf("an unrouted schema against one destination: %q, %v", got, err)
	}
	if got, err := one.resolve("quiet-year", "library"); err != nil || got != "http://library:4200" {
		t.Errorf("a schema naming the only destination: %q, %v", got, err)
	}

	two, err := loadDestinations(writeDestinations(t, "library: http://library:4200\nevent: http://event:4200\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := two.resolve("spring-ctf", "event"); err != nil || got != "http://event:4200" {
		t.Errorf("a routed schema: %q, %v", got, err)
	}
	// The accident this exists to refuse: an event's schema landing on the
	// year-round orchestrator because a line was forgotten.
	_, err = two.resolve("spring-ctf", "")
	if err == nil {
		t.Fatal("an unrouted schema was accepted with two destinations configured")
	}
	for _, want := range []string{"spring-ctf", "'event'", "'library'"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %s", want, err)
		}
	}
	// A typo is refused against the list rather than dialled.
	_, err = two.resolve("spring-ctf", "evnet")
	if err == nil || !strings.Contains(err.Error(), "'event'") {
		t.Errorf("a mistyped destination: %v", err)
	}

	none, err := loadDestinations("")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := none.resolve("any", ""); err == nil || !strings.Contains(err.Error(), DESTINATIONS_ENV) {
		t.Errorf("no destinations configured at all: %v", err)
	}
	if _, err := none.resolve("any", "library"); err == nil || !strings.Contains(err.Error(), DESTINATIONS_ENV) {
		t.Errorf("a named destination with none configured: %v", err)
	}
}
