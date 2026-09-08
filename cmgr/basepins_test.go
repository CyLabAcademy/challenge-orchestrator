package cmgr

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const ubuntuDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
const nginxDigest = "sha256:2222222222222222222222222222222222222222222222222222222222222222"

func testPins() map[string]string {
	return map[string]string{
		"ubuntu:24.04":   ubuntuDigest,
		"nginx:mainline": nginxDigest,
		"busybox:latest": "sha256:3333333333333333333333333333333333333333333333333333333333333333",
	}
}

func TestPinDockerfileRewritesOnlyExternalBases(t *testing.T) {
	// The shape every built-in template and most challenges use: an external
	// base aliased as a stage, then a second stage built FROM that alias.
	in := []byte("# a comment\n" +
		"FROM ubuntu:24.04 AS base\n" +
		"ENV DEBIAN_FRONTEND=noninteractive\n" +
		"RUN apt-get update\n" +
		"\n" +
		"FROM base AS challenge\n" +
		"ARG FLAG\n" +
		"RUN echo $FLAG\n")

	out, applied, _ := pinDockerfile(in, testPins())
	got := string(out)

	if !strings.Contains(got, "FROM ubuntu@"+ubuntuDigest+" AS base") {
		t.Errorf("external base was not pinned:\n%s", got)
	}
	if !strings.Contains(got, "FROM base AS challenge") {
		t.Errorf("the internal stage reference must be left alone:\n%s", got)
	}
	if len(applied) != 1 || applied[0] != "ubuntu:24.04" {
		t.Errorf("applied = %v, want [ubuntu:24.04]", applied)
	}
	// Everything else must survive byte for byte.
	for _, line := range []string{"# a comment", "ENV DEBIAN_FRONTEND=noninteractive", "RUN apt-get update", "ARG FLAG"} {
		if !strings.Contains(got, line) {
			t.Errorf("lost line %q", line)
		}
	}
}

func TestPinDockerfileLeavesTheseAlone(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"unpinned image not in the map", "FROM debian:12\nRUN true\n"},
		{"already a digest", "FROM ubuntu@sha256:abc AS base\nRUN true\n"},
		{"build-arg driven", "ARG BASE\nFROM $BASE AS base\nRUN true\n"},
		{"scratch", "FROM scratch\nCOPY x /\n"},
		{"stage alias shadowing an image name", "FROM ubuntu:24.04 AS ubuntu\nFROM ubuntu AS second\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, applied, _ := pinDockerfile([]byte(tc.in), testPins())
			if tc.name == "stage alias shadowing an image name" {
				// The first FROM is a real image and is pinned; the second
				// names the stage and must not be.
				if strings.Count(string(out), "@sha256:") != 1 {
					t.Errorf("expected exactly one pin:\n%s", out)
				}
				if !strings.Contains(string(out), "FROM ubuntu AS second") {
					t.Errorf("the stage reference was rewritten:\n%s", out)
				}
				return
			}
			if string(out) != tc.in {
				t.Errorf("Dockerfile changed but should not have:\nbefore: %q\nafter:  %q", tc.in, out)
			}
			if len(applied) != 0 {
				t.Errorf("applied = %v, want none", applied)
			}
		})
	}
}

func TestPinDockerfilePreservesFlagsAndCase(t *testing.T) {
	in := []byte("from --platform=linux/amd64 ubuntu:24.04 as base\nRUN true\n")
	out, applied, _ := pinDockerfile(in, testPins())
	got := string(out)
	if !strings.Contains(got, "--platform=linux/amd64 ubuntu@"+ubuntuDigest) {
		t.Errorf("the platform flag or the pin was mangled:\n%s", got)
	}
	if !strings.HasSuffix(strings.SplitN(got, "\n", 2)[0], " as base") {
		t.Errorf("the stage clause was not preserved verbatim:\n%s", got)
	}
	if len(applied) != 1 {
		t.Errorf("applied = %v, want one entry", applied)
	}
}

func TestPinDockerfileUntaggedReferenceUsesLatest(t *testing.T) {
	out, applied, _ := pinDockerfile([]byte("FROM busybox\nRUN true\n"), testPins())
	if !strings.Contains(string(out), "FROM busybox@sha256:3333") {
		t.Errorf("an untagged reference should match the :latest pin:\n%s", out)
	}
	if len(applied) != 1 {
		t.Errorf("applied = %v, want one entry", applied)
	}
}

func TestPinDockerfileNoPinsIsIdentity(t *testing.T) {
	in := []byte("FROM ubuntu:24.04\nRUN true\n")
	out, applied, _ := pinDockerfile(in, map[string]string{})
	if string(out) != string(in) || applied != nil {
		t.Errorf("with no pins the Dockerfile must pass through untouched")
	}
}

func TestExternalBaseRefs(t *testing.T) {
	df := []byte("FROM ubuntu:24.04 AS base\n" +
		"FROM base AS challenge\n" +
		"FROM nginx:mainline AS web\n" +
		"FROM alpine@sha256:dead AS pinned\n" +
		"FROM $ARGBASE AS argy\n")
	got := externalBaseRefs(df)
	want := map[string]bool{"ubuntu:24.04": true, "nginx:mainline": true}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for ref := range want {
		if !got[ref] {
			t.Errorf("missing %s from %v", ref, got)
		}
	}
}

func TestReadBasePins(t *testing.T) {
	dir := t.TempDir()

	// A missing file is no pins, not an error.
	pins, err := readBasePins(filepath.Join(dir, "absent.json"))
	if err != nil || len(pins) != 0 {
		t.Fatalf("missing file: got %v, %v", pins, err)
	}

	// An empty file likewise.
	empty := filepath.Join(dir, "empty.json")
	os.WriteFile(empty, []byte("  \n"), 0o644)
	if pins, err = readBasePins(empty); err != nil || len(pins) != 0 {
		t.Fatalf("empty file: got %v, %v", pins, err)
	}

	good := filepath.Join(dir, "good.json")
	os.WriteFile(good, []byte(`{"ubuntu:24.04": "`+ubuntuDigest+`"}`), 0o644)
	if pins, err = readBasePins(good); err != nil || pins["ubuntu:24.04"] != ubuntuDigest {
		t.Fatalf("good file: got %v, %v", pins, err)
	}

	// A value that is not a digest is a hard error: silently falling back to a
	// mutable tag defeats the point.
	bad := filepath.Join(dir, "bad.json")
	os.WriteFile(bad, []byte(`{"ubuntu:24.04": "24.04"}`), 0o644)
	if _, err = readBasePins(bad); err == nil {
		t.Fatalf("a non-digest pin value must be rejected")
	}
}

func TestRefHelpers(t *testing.T) {
	cases := []struct {
		ref    string
		hasTag bool
		name   string
	}{
		{"ubuntu:24.04", true, "ubuntu"},
		{"ubuntu", false, "ubuntu"},
		{"registry.example:5000/base/foo:v1", true, "registry.example:5000/base/foo"},
		{"registry.example:5000/base/foo", false, "registry.example:5000/base/foo"},
	}
	for _, c := range cases {
		if got := refHasTag(c.ref); got != c.hasTag {
			t.Errorf("refHasTag(%q) = %v, want %v", c.ref, got, c.hasTag)
		}
		if got := refName(c.ref); got != c.name {
			t.Errorf("refName(%q) = %q, want %q", c.ref, got, c.name)
		}
	}
}

// A build-arg reference is not a literal one, so neither pinning nor a refresh
// may treat it as an image. The corpus contains none of these today -- the
// forms below are the shapes a Dockerfile can legally use, not a census -- and
// that is exactly why this is a test: the first one someone writes must be
// skipped rather than resolved against a registry as the literal string.
func TestBuildArgReferencesAreNeverTreatedAsImages(t *testing.T) {
	refs := []string{
		"$BASE",
		"ubuntu:${UBUNTU_TAG}",
		"php:${PHP_SERIES}-apache",
		"nginx:${NGINX_TAG}",
		"localhost/cmgr-base/py-build:${BASE_TAG}",
	}
	for _, ref := range refs {
		df := []byte("FROM " + ref + " AS base\nRUN true\n")

		out, applied, _ := pinDockerfile(df, testPins())
		if string(out) != string(df) || len(applied) != 0 {
			t.Errorf("%s: must not be rewritten, got %q", ref, out)
		}
		if got := externalBaseRefs(df); len(got) != 0 {
			t.Errorf("%s: must not be offered for pinning, got %v", ref, got)
		}
	}
}

// The fleet's dominant shape, verified against the real corpus: one external
// base aliased as a stage, with most Dockerfiles then building FROM the alias.
func TestRealFleetShapes(t *testing.T) {
	pins := map[string]string{"ubuntu:24.04": ubuntuDigest}
	df := []byte("FROM ubuntu:24.04 AS base\n" +
		"RUN apt-get update\n" +
		"FROM base AS builder_base\n" +
		"RUN make\n" +
		"FROM builder_base AS challenge\n" +
		"RUN true\n")
	out, applied, _ := pinDockerfile(df, pins)
	got := string(out)
	if strings.Count(got, "@sha256:") != 1 {
		t.Fatalf("exactly one FROM should be pinned:\n%s", got)
	}
	if !strings.Contains(got, "FROM base AS builder_base") || !strings.Contains(got, "FROM builder_base AS challenge") {
		t.Errorf("chained stage aliases must survive untouched:\n%s", got)
	}
	if len(applied) != 1 || applied[0] != "ubuntu:24.04" {
		t.Errorf("applied = %v", applied)
	}
}

// A base bump has to reach the content identity, or refreshing pins would
// leave every challenge sitting on the image it already had.
func TestPinChecksumChangesContentIdentity(t *testing.T) {
	unpinned := &basePins{m: map[string]string{}}
	if got := unpinned.checksum(); got != 0 {
		t.Fatalf("no pins must fingerprint as 0, got %d", got)
	}

	a := &basePins{m: map[string]string{"ubuntu:24.04": ubuntuDigest}}
	b := &basePins{m: map[string]string{"ubuntu:24.04": ubuntuDigest, "nginx:mainline": nginxDigest}}
	moved := &basePins{m: map[string]string{"ubuntu:24.04": nginxDigest}}

	if a.checksum() == 0 || a.checksum() != a.checksum() {
		t.Fatalf("a pin fingerprint must be non-zero and stable")
	}
	if a.checksum() == moved.checksum() {
		t.Errorf("moving a base must change the fingerprint")
	}
	if a.checksum() == b.checksum() {
		t.Errorf("adding a pin must change the fingerprint")
	}

	// Turning pinning on re-stamps builds; leaving it off preserves whatever
	// checksums an existing database already holds.
	plain := contentChecksum(0xdeadbeef, "flag{%s}", 0)
	if plain != contentChecksum(0xdeadbeef, "flag{%s}", 0) {
		t.Fatalf("contentChecksum must be deterministic")
	}
	if plain == contentChecksum(0xdeadbeef, "flag{%s}", a.checksum()) {
		t.Errorf("pinned and unpinned builds must not share a content identity")
	}
	if contentChecksum(0xdeadbeef, "flag{%s}", a.checksum()) ==
		contentChecksum(0xdeadbeef, "flag{%s}", moved.checksum()) {
		t.Errorf("a moved base must produce a different content identity")
	}
}

// A repository holds Dockerfiles that are not challenge roots: a web
// challenge's app/ or bot/ subdirectory, and a directory of hand-maintained
// base images. cork builds none of them, so pinning their bases would put
// references into the fingerprint that no build consumes -- and moving one
// would then re-stamp the identity of every challenge in the fleet.
func TestScanBaseRefsOnlyCountsChallengeRoots(t *testing.T) {
	root := t.TempDir()

	write := func(rel, contents string) {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %s", rel, err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			t.Fatalf("write %s: %s", rel, err)
		}
	}

	// Two real challenges, one of them naming the same base twice.
	write("cat/one/problem.md", "# one\n")
	write("cat/one/Dockerfile", "FROM ubuntu:24.04\nRUN apt-get update\n")
	write("cat/two/problem.md", "# two\n")
	write("cat/two/Dockerfile", "FROM ubuntu:24.04\n")

	// A subdirectory of a challenge, built by the challenge's own tooling.
	write("cat/two/app/Dockerfile", "FROM node:18\n")
	// A directory of base images kept beside the challenges.
	write("docker-bases/py/Dockerfile", "FROM python:3.12-slim\n")
	// A hidden directory is skipped outright.
	write(".cache/scratch/problem.md", "# scratch\n")
	write(".cache/scratch/Dockerfile", "FROM alpine:3.20\n")

	// The three built-in challenge types ship their own Dockerfiles and are
	// always counted, so measure the challenge contribution as a delta.
	empty := &Manager{chalDir: t.TempDir(), log: newLogger(DISABLED)}
	builtin, err := empty.scanBaseRefs()
	if err != nil {
		t.Fatalf("scanBaseRefs over an empty directory failed: %s", err)
	}

	m := &Manager{chalDir: root, log: newLogger(DISABLED)}
	refs, err := m.scanBaseRefs()
	if err != nil {
		t.Fatalf("scanBaseRefs failed: %s", err)
	}

	if got, want := refs["ubuntu:24.04"]-builtin["ubuntu:24.04"], 2; got != want {
		t.Errorf("challenge roots contributed %d uses of ubuntu:24.04, want %d", got, want)
	}
	for _, ref := range []string{"node:18", "python:3.12-slim", "alpine:3.20"} {
		if _, ok := refs[ref]; ok {
			t.Errorf("%s was pinned, but no challenge root builds on it", ref)
		}
	}
}

// The parser guards. Each of these was reachable and silently wrong before the
// FROM scan was shared between pinDockerfile and externalBaseRefs.

func TestScanSkipsScratch(t *testing.T) {
	// `scratch` is not an image: `docker pull scratch` is refused outright, so
	// offering it to the registry makes pin-refresh report a permanent partial
	// failure on any corpus that contains one.
	in := []byte("FROM scratch AS empty\nCOPY --from=build /a /a\nFROM ubuntu:24.04\n")
	refs := externalBaseRefs(in)
	if refs["scratch"] {
		t.Errorf("scratch was offered as an image reference: %v", refs)
	}
	if !refs["ubuntu:24.04"] {
		t.Errorf("the real base was lost: %v", refs)
	}
}

func TestScanRejectsTokensThatCannotBeReferences(t *testing.T) {
	// `FROM \\` continues onto the next line. The bare backslash used to be
	// scanned as an image name and written into the pin map.
	// Every one of these used to be offered to the registry as an image name.
	// The test asserts on a NON-EMPTY set on purpose: the first version of it
	// ranged over an empty map, so its body never ran and it asserted nothing.
	in := []byte("FROM \\\n    ubuntu:24.04 AS base\n" +
		"FROM nginx:mainline AS real\n")
	refs := externalBaseRefs(in)
	if len(refs) == 0 {
		t.Fatal("no references at all: the fixture no longer exercises anything")
	}
	if !refs["nginx:mainline"] {
		t.Errorf("the real reference was lost: %v", refs)
	}
	for ref := range refs {
		if !refRe.MatchString(ref) {
			t.Errorf("scanned a token that cannot be an image reference: %q", ref)
		}
	}
}

func TestPinDockerfileLeavesAContinuedFromAlone(t *testing.T) {
	// The reference is real, but the instruction does not sit on one line, so
	// the rewriter declines rather than guessing where to put the digest.
	in := []byte("FROM ubuntu:24.04 \\\n    AS base\nRUN true\n")
	out, applied, _ := pinDockerfile(in, testPins())
	if len(applied) != 0 {
		t.Errorf("applied = %v, want nothing rewritten across a continuation", applied)
	}
	if string(out) != string(in) {
		t.Errorf("the Dockerfile was modified:\n%s", out)
	}
	// The other half, and the half that was missing: declining to REWRITE the
	// instruction must not mean declining to RESOLVE its base. Dropping the
	// reference from the scan instead of marking it unrewritable leaves the
	// challenge on a mutable tag with nothing saying so, and every test here
	// still passed.
	if !externalBaseRefs(in)["ubuntu:24.04"] {
		t.Error("the base of a continued FROM was not offered for resolution: it is a real reference, and pin-refresh must still see it")
	}
}

func TestPinDockerfilePreservesCRLF(t *testing.T) {
	// Three challenges in the real corpus carry CR in the committed blob, so
	// the builder really does see CRLF. It works only because \\r is whitespace
	// to RE2; a future tightening of fromLineRe would unpin them silently.
	in := []byte("FROM ubuntu:24.04 AS base\r\nRUN apt-get update\r\n")
	out, applied, _ := pinDockerfile(in, testPins())
	got := string(out)

	if len(applied) != 1 || applied[0] != "ubuntu:24.04" {
		t.Fatalf("applied = %v, want the base pinned", applied)
	}
	if !strings.Contains(got, "FROM ubuntu@"+ubuntuDigest+" AS base\r\n") {
		t.Errorf("CRLF was not preserved through the rewrite: %q", got)
	}
	if strings.Count(got, "\r") != strings.Count(string(in), "\r") {
		t.Errorf("carriage returns changed: in %d, out %d",
			strings.Count(string(in), "\r"), strings.Count(got, "\r"))
	}
}

func TestWriteBasePinsReplacesAndRoundTrips(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "base-pins.json")

	if err := writeBasePins(path, testPins()); err != nil {
		t.Fatalf("writeBasePins: %s", err)
	}
	got, err := readBasePins(path)
	if err != nil {
		t.Fatalf("readBasePins: %s", err)
	}
	if got["ubuntu:24.04"] != ubuntuDigest {
		t.Errorf("round trip lost a pin: %v", got)
	}

	// A rewrite must replace the file, not accumulate temporaries beside it: a
	// leaked temp per refresh is a slow leak in the challenge directory.
	if err := writeBasePins(path, map[string]string{"alpine:3.20": ubuntuDigest}); err != nil {
		t.Fatalf("second writeBasePins: %s", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "base-pins.json" {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("expected only the pin file, found %v", names)
	}
	got, err = readBasePins(path)
	if err != nil {
		t.Fatalf("readBasePins after rewrite: %s", err)
	}
	if _, stale := got["ubuntu:24.04"]; stale {
		t.Errorf("the rewrite did not replace the file: %v", got)
	}

	// The pin file is committed to the challenge repository and read by
	// whatever user cmgrd runs as. os.CreateTemp makes its file 0600, so the
	// explicit Chmod is load-bearing -- without it the rename installs a pin
	// file no one else can read, which the previous plain-WriteFile
	// implementation never did.
	//
	// Skipped on Windows, whose permission bits do not survive this round trip.
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o644 {
			t.Errorf("pin file mode = %#o, want 0644: CreateTemp's 0600 was not restored", perm)
		}
	}
}

// Note on the name: nothing above observes atomicity, and nothing here can
// without killing the process mid-write. Replacing the temp-file-and-rename
// body with a plain os.WriteFile still passes everything except the mode check.
// What this does pin down is that a rewrite REPLACES rather than merges, and
// leaves no temp file behind.

// Heredocs are not modelled; they are refused. See heredocMarker.
//
// Every shape below was a way for a body line to be rewritten or for a good
// file to be mis-parsed under the two regexp models that preceded this. They
// are kept as a table because the point is that ALL of them take the same
// path now, including the ones that are not heredocs at all.
func TestScanRefusesAnythingWithAHeredocMarker(t *testing.T) {
	pins := testPins()
	pins["redis"] = "sha256:4444444444444444444444444444444444444444444444444444444444444444"

	for _, tc := range []struct{ name, df string }{
		{"plain", "FROM ubuntu:24.04\nRUN python3 <<EOF\nfrom redis import Redis\nEOF\n"},
		{"whitespace after the marker", "FROM ubuntu:24.04\nRUN cat << EOF\nfrom redis import Redis\nEOF\n"},
		{"empty-quote delimiter", "FROM ubuntu:24.04\nRUN python3 <<\"\"EOF\nfrom redis import Redis\nEOF\n"},
		{"quoted delimiter with punctuation", "FROM ubuntu:24.04\nRUN python3 <<'PY-EOF'\nfrom redis import Redis\nPY-EOF\n"},
		{"terminator with trailing whitespace", "FROM ubuntu:24.04\nCOPY <<EOF /app/s.py\nimport os\nEOF \nfrom redis import Redis\nEOF\n"},
		{"opened on a continuation line", "FROM ubuntu:24.04\nRUN apt-get update && \\\n    python3 <<EOF\nfrom redis import Redis\nEOF\n"},
		{"bash here-string", "FROM ubuntu:24.04\nRUN grep -q x <<<\"hello\"\n"},
		{"not a heredoc at all, but refused anyway", "FROM ubuntu:24.04\nRUN echo 'cout<<flag' > /a.cpp\n"},
		{"marker split across a continuation", "FROM ubuntu:24.04\nRUN cat <\\\n<EOF\nfrom redis import Redis\nEOF\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, applied, clean := pinDockerfile([]byte(tc.df), pins)
			if clean {
				t.Errorf("file was parsed rather than refused")
			}
			if len(applied) != 0 {
				t.Errorf("applied = %v, want nothing rewritten in a refused file", applied)
			}
			if string(out) != tc.df {
				t.Errorf("a refused file was modified:\n%s", out)
			}
			if refs := externalBaseRefs([]byte(tc.df)); len(refs) != 0 {
				t.Errorf("harvested %v from a refused file: pin-refresh would resolve those and the rewriter would then edit the source", refs)
			}
		})
	}
}

// Line continuations, with no heredoc involved. BuildKit keeps an open
// continuation running across comment and blank lines rather than ending it,
// and an earlier version of this scanner ended it early -- which exposed the
// rest of a RUN body as Dockerfile.
func TestScanContinuationsAndComments(t *testing.T) {
	pins := testPins()
	pins["redis"] = "sha256:4444444444444444444444444444444444444444444444444444444444444444"

	for _, tc := range []struct {
		name, df string
		applied  []string
	}{
		{
			name:    "a continuation line that looks like an instruction",
			df:      "FROM ubuntu:24.04 AS base\nRUN echo \"import x \\\n    from redis import Redis\" > app.py\nFROM nginx:mainline\n",
			applied: []string{"nginx:mainline", "ubuntu:24.04"},
		},
		{
			name:    "a comment inside a continuation does not end it",
			df:      "FROM ubuntu:24.04 AS base\nRUN echo a && \\\n# explain\n    from redis import Redis\nFROM nginx:mainline\n",
			applied: []string{"nginx:mainline", "ubuntu:24.04"},
		},
		{
			name:    "a blank line inside a continuation does not end it",
			df:      "FROM ubuntu:24.04 AS base\nRUN echo a && \\\n\n    from redis import Redis\nFROM nginx:mainline\n",
			applied: []string{"nginx:mainline", "ubuntu:24.04"},
		},
		{
			// Guarded by fromLineRe's ^ anchor, not by the continuation logic:
			// every other row here puts this shape ACROSS a continuation, so
			// un-anchoring the pattern went unnoticed.
			name:    "an instruction-shaped string inside a single-line RUN",
			df:      "FROM ubuntu:24.04 AS base\nRUN echo \"from redis import Redis\" > app.py\nFROM nginx:mainline\n",
			applied: []string{"nginx:mainline", "ubuntu:24.04"},
		},
		{
			name:    "a comment ending in a backslash continues nothing",
			df:      "FROM ubuntu:24.04 AS base\n# note \\\nFROM nginx:mainline\n",
			applied: []string{"nginx:mainline", "ubuntu:24.04"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, applied, clean := pinDockerfile([]byte(tc.df), pins)
			if !clean {
				t.Fatal("file was refused; none of these contain a heredoc marker")
			}
			if strings.Join(applied, ",") != strings.Join(tc.applied, ",") {
				t.Errorf("applied = %v, want %v", applied, tc.applied)
			}
			if externalBaseRefs([]byte(tc.df))["redis"] {
				t.Error("harvested redis from inside a RUN body")
			}
		})
	}
}

// Stage names are case-insensitive to BuildKit. Folding only one side would
// make `FROM base` after `AS Base` an external image: resolved against a
// registry, written into the pin map, and rewritten in the build context.
func TestScanFoldsStageNameCase(t *testing.T) {
	in := []byte("FROM ubuntu:24.04 AS Base\nRUN true\nFROM BASE AS challenge\nRUN true\n")
	_, applied, clean := pinDockerfile(in, testPins())
	if !clean {
		t.Fatal("unexpected refusal")
	}
	if strings.Join(applied, ",") != "ubuntu:24.04" {
		t.Errorf("applied = %v, want only the external base", applied)
	}
	for ref := range externalBaseRefs(in) {
		if strings.EqualFold(ref, "base") {
			t.Errorf("stage alias %q was harvested as an image reference", ref)
		}
	}
}

// The refresh loop stops when less than one worst-case reference-resolution
// remains, so the budget must exceed that reserve or the very first iteration
// gives up having done nothing. Folding the reserve into the floor made the two
// equal, and pin-refresh returned "0 of 2 references re-resolved" against a
// live registry -- caught by the e2e, which is six minutes later than here.
func TestBasePinsRefreshBudgetLeavesRoomToWork(t *testing.T) {
	for _, tc := range []struct {
		name string
		n    int
		per  time.Duration
	}{
		{"the e2e: two references, short timeout", 2, 10 * time.Second},
		{"one reference", 1, 30 * time.Second},
		{"none at all", 0, 30 * time.Second},
		{"the real corpus", 32, 30 * time.Second},
		{"every challenge with its own base", 400, 30 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// timing() falls back to defaults unless pollInterval is set,
			// so both fields are needed to pin controlTimeout.
			m := &Manager{workerTiming: workerTiming{
				pollInterval:   time.Second,
				controlTimeout: tc.per,
			}}
			budget := m.basePinsRefreshBudget(tc.n)
			reserve := 2 * tc.per
			if budget <= reserve {
				t.Errorf("budget %s does not exceed the per-reference reserve %s: the loop would give up before resolving anything", budget, reserve)
			}
		})
	}
}
