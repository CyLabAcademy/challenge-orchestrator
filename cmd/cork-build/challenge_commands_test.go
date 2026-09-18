package main

import (
	"flag"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Asking a command for help is not getting it wrong. flag.Parse reports
// both as an error, and treating -h as a usage failure would make
// `cork-build list --help` exit non-zero in a script.
func TestParseCommandFlags(t *testing.T) {
	quiet := func(name string) *flag.FlagSet {
		set := commandFlags(name, "")
		set.SetOutput(io.Discard)
		return set
	}

	set := quiet("list")
	set.Bool("verbose", false, "")
	if code, ok := parseCommandFlags(set, []string{"--verbose"}); !ok || code != NO_ERROR {
		t.Errorf("a good parse: %d, %v", code, ok)
	}

	if code, ok := parseCommandFlags(quiet("list"), []string{"--help"}); ok || code != NO_ERROR {
		t.Errorf("--help answered %d (continue=%v), want a clean stop", code, ok)
	}
	if code, ok := parseCommandFlags(quiet("list"), []string{"--nonsense"}); ok || code != USAGE_ERROR {
		t.Errorf("an unknown flag answered %d (continue=%v), want a usage error", code, ok)
	}
}

// A schema name goes into a request path exactly as written, so what cannot
// address a schema is refused where the name is read rather than where it
// silently addresses something else.
func TestValidSchemaName(t *testing.T) {
	// A name is escaped on the way out and decoded by the daemon, so these
	// survive the round trip whole -- measured against a real handler, not
	// assumed. Refusing a space would have refused a working schema.
	for _, good := range []string{"spring", "spring-ctf", "year_round", "2026.spring", "a", "Spring CTF 2026", "año"} {
		if err := validSchemaName(good); err != nil {
			t.Errorf("%q was refused, and it addresses a schema perfectly well: %s", good, err)
		}
	}
	// These re-divide the request ('/' and an encoded one leave the last
	// segment, '?' and '#' the first) or are refused by net/url outright.
	for _, bad := range []string{"", "a/b", "a?b", "a#b", "a%2Fb", "a\tb", "a\nb", "a\\b"} {
		if err := validSchemaName(bad); err == nil {
			t.Errorf("%q was accepted, and it cannot address a schema over HTTP", bad)
		}
	}
	// A name that reads as a file could never be removed, since remove-schema
	// refuses those as a caller passing the file.
	for _, bad := range []string{"spring.yaml", "spring.YML", "spring.json"} {
		if err := validSchemaName(bad); err == nil {
			t.Errorf("%q was accepted, and remove-schema would refuse it as a file", bad)
		}
	}
	// The complaint says only what is wrong; the caller frames it, since one
	// is reading a file and the others an argument.
	if err := validSchemaName(""); err == nil || !strings.Contains(err.Error(), "no name") {
		t.Errorf("the empty name is refused with %v", err)
	}
	if err := validSchemaName("a/b"); err == nil || !strings.Contains(err.Error(), "a/b") {
		t.Errorf("the refusal does not quote the name: %v", err)
	}
}

// The type line in a problem.md is a yaml list item, and convert-to-custom
// has to find it, read it and replace it. Worth testing on its own: it is
// the only real parsing in these commands, and getting it wrong rewrites a
// challenge's source -- which changes the identity of every build of it.
func TestChallengeTypeLine(t *testing.T) {
	problem := "# BinEx101\n\n- namespace: cmgr/examples\n- type: remote-make\n- category: Binary Exploitation\n"
	match := challengeTypeLine.FindSubmatch([]byte(problem))
	if match == nil {
		t.Fatal("the type line was not found in a problem.md that has one")
	}
	if got := string(match[2]); got != "remote-make" {
		t.Errorf("read the type as %q", got)
	}

	converted := string(challengeTypeLine.ReplaceAll([]byte(problem), []byte("\n$1 custom")))
	if !strings.Contains(converted, "- type: custom") {
		t.Errorf("the type was not rewritten: %q", converted)
	}
	// Everything else survives, including the indentation of the line that
	// was replaced: $1 carries it.
	for _, want := range []string{"# BinEx101", "- namespace: cmgr/examples", "- category: Binary Exploitation"} {
		if !strings.Contains(converted, want) {
			t.Errorf("conversion lost %q: %q", want, converted)
		}
	}
	if strings.Contains(converted, "remote-make") {
		t.Errorf("the old type is still there: %q", converted)
	}
	// The reason the file is written whole rather than over: every built-in
	// type name is longer than 'custom', so an in-place write that did not
	// truncate would leave the tail of the original behind.
	if len(converted) >= len(problem) {
		t.Errorf("conversion did not shorten the file (%d -> %d), so the truncation this guards against would not show up here",
			len(problem), len(converted))
	}

	// A problem.md with no type declaration is not converted by accident.
	if challengeTypeLine.FindSubmatch([]byte("# Nothing\n\n- namespace: x\n")) != nil {
		t.Error("a problem.md declaring no type matched the type line")
	}
}

// convert-to-custom will not write over a Dockerfile that is already there.
// That file would be the challenge's own, and overwriting it with the
// built-in one is not a conversion, it is a loss.
func TestConvertToCustomKeepsAnExistingDockerfile(t *testing.T) {
	dir := t.TempDir()
	problem := filepath.Join(dir, "problem.md")
	if err := os.WriteFile(problem, []byte("# X\n\n- type: remote-make\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mine := filepath.Join(dir, "Dockerfile")
	if err := os.WriteFile(mine, []byte("FROM scratch\n# mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// No manager is needed to get this far, and that is the point: the
	// refusal comes before anything is read out of one or written to disk.
	if code := convertToCustomCommand(nil, []string{dir}); code == NO_ERROR {
		t.Error("convert-to-custom wrote over a Dockerfile that was already there")
	}
	kept, err := os.ReadFile(mine)
	if err != nil || !strings.Contains(string(kept), "# mine") {
		t.Errorf("the challenge's own Dockerfile did not survive: %q, %v", kept, err)
	}
	unchanged, err := os.ReadFile(problem)
	if err != nil || !strings.Contains(string(unchanged), "remote-make") {
		t.Errorf("problem.md was rewritten although the conversion was refused: %q, %v", unchanged, err)
	}
}

// convert-to-custom rewrites somebody's source, so the write is atomic: the
// challenge is either converted or untouched, never truncated. The mode has
// to survive it too -- a rename replaces the file, and CreateTemp's 0600
// would leave a problem.md readable only by whoever ran the conversion.
func TestWriteThenRename(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "problem.md")
	if err := os.WriteFile(path, []byte("original, and longer\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeThenRename(path, []byte("short\n")); err != nil {
		t.Fatalf("replacing: %s", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "short\n" {
		t.Errorf("content is %q (%v): a shorter replacement must not leave the tail of the original", got, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o644 {
		t.Errorf("mode is %v, want the 0644 the file already had", info.Mode().Perm())
	}
	// Nothing is left beside it: a temporary that outlived the rename would
	// sit in the challenge directory and reach the next source checksum.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("the directory holds %v, want problem.md alone", names)
	}
}

// A challenge repo authored on Windows has CRLF problem.md files, and the
// type pattern runs to the end of the line. Untrimmed, the type would be
// "remote-make\r" -- a type nothing builds for -- and an already-converted
// challenge would read as "custom\r" and miss its own guard.
func TestChallengeTypeLineTrimsLineEndings(t *testing.T) {
	for _, tc := range []struct{ name, problem, want string }{
		{"crlf", "# X\r\n\r\n- type: remote-make\r\n", "remote-make"},
		{"trailing spaces", "# X\n\n- type: remote-make   \n", "remote-make"},
		{"already custom, crlf", "# X\r\n\r\n- type: custom\r\n", "custom"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			match := challengeTypeLine.FindSubmatch([]byte(tc.problem))
			if match == nil {
				t.Fatal("no type line found")
			}
			if got := strings.TrimSpace(string(match[2])); got != tc.want {
				t.Errorf("read the type as %q, want %q", got, tc.want)
			}
		})
	}
}
