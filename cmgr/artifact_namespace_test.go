package cmgr

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// namespaceManager is a local build plane with a resolved artifact
// directory, which is what a build plane has and an orchestrator does not.
func namespaceManager(t *testing.T) (*Manager, string) {
	t.Helper()
	dir := t.TempDir()
	m := &Manager{log: captureLog(&bytes.Buffer{}), artifactsDir: dir}
	return m, dir
}

// bundleOf is the file a build of this schema and id keeps its bundle in.
func bundleOf(schema string, id BuildId) *BuildMetadata {
	return &BuildMetadata{Id: id, Schema: schema}
}

// Each schema's bundles go in the directory its destination names, the
// directories are made up front, and a schema with no destination keeps the
// artifact directory itself.
func TestSetArtifactNamespaces(t *testing.T) {
	m, base := namespaceManager(t)

	if err := m.SetArtifactNamespaces(map[string]string{
		"spring":  "library",
		"autumn":  "event",
		"neither": "",
	}); err != nil {
		t.Fatalf("SetArtifactNamespaces: %s", err)
	}
	for schema, want := range map[string]string{
		"spring":  filepath.Join(base, "library"),
		"autumn":  filepath.Join(base, "event"),
		"neither": base,
		"unknown": base,
	} {
		if got := m.artifactDirFor(schema); got != want {
			t.Errorf("schema '%s' writes to %s, want %s", schema, got, want)
		}
	}
	// Made now rather than at the first build, so a directory that cannot be
	// created is reported before an event's worth of building.
	for _, name := range []string{"library", "event"} {
		if info, err := os.Stat(filepath.Join(base, name)); err != nil || !info.IsDir() {
			t.Errorf("the %s namespace was not created: %v", name, err)
		}
	}
}

// A destination name is a directory name, so anything that would escape the
// artifact directory is refused rather than cleaned into something else. A
// destination comes out of CORK_DESTINATIONS, which an operator writes.
func TestSetArtifactNamespacesRefuseAPath(t *testing.T) {
	for _, name := range []string{"..", ".", "a/b", "../escape", "/absolute", "a/"} {
		t.Run(name, func(t *testing.T) {
			m, base := namespaceManager(t)
			err := m.SetArtifactNamespaces(map[string]string{"spring": name})
			if err == nil {
				t.Fatalf("namespace %q was accepted, resolving to %s", name, m.artifactDirFor("spring"))
			}
			if !strings.Contains(err.Error(), "spring") {
				t.Errorf("the refusal does not name the schema: %s", err)
			}
			if m.artifactDirFor("spring") != base {
				t.Errorf("a refused namespace moved the directory to %s", m.artifactDirFor("spring"))
			}
		})
	}
}

// An orchestrator resolves no artifact directory at all, so there is nothing
// for a namespace to be under. Nothing is built there either, which is why
// this is not an error: it is the same no-op as the directory it never made.
func TestSetArtifactNamespacesOnAnExternalPlane(t *testing.T) {
	m := &Manager{log: captureLog(&bytes.Buffer{}), externalBuildPlane: true}
	if err := m.SetArtifactNamespaces(map[string]string{"spring": "library"}); err != nil {
		t.Fatalf("SetArtifactNamespaces on an external build plane: %s", err)
	}
	if m.artifactsDir != "" {
		t.Errorf("an external build plane resolved an artifact directory: %q", m.artifactsDir)
	}
}

// A rebuild goes where the generation it replaces is, not where this process
// thinks the schema belongs. A build plane is only told the namespaces of
// the schemas one command was given, while an update rebuilds every build of
// a challenge whose source moved -- schemas it was not given included. Those
// rebuilds landing in the artifact directory itself would leave their
// destination's artifact server publishing the generation before, silently.
func TestArtifactDirForBuildFollowsTheBundleItReplaces(t *testing.T) {
	m, base := namespaceManager(t)
	if err := m.SetArtifactNamespaces(map[string]string{"spring": "library"}); err != nil {
		t.Fatal(err)
	}
	// Built once, by a command that was given this schema.
	first := bundleOf("spring", 7)
	if got, want := m.artifactDirForBuild(first), filepath.Join(base, "library"); got != want {
		t.Fatalf("a first build goes to %s, want %s", got, want)
	}
	if err := os.WriteFile(filepath.Join(base, "library", "7.tar.gz"), []byte("gz"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Rebuilt by a command that was given some other schema, so this one's
	// namespace is unknown to it.
	other, _ := namespaceManager(t)
	other.artifactsDir = base
	if err := other.SetArtifactNamespaces(map[string]string{"autumn": "event"}); err != nil {
		t.Fatal(err)
	}
	if got, want := other.artifactDirForBuild(first), filepath.Join(base, "library"); got != want {
		t.Errorf("a rebuild by a command that was not given this schema goes to %s, want %s: the bundle would stop being published where its players look", got, want)
	}
	// A build with no bundle anywhere still goes where its schema says.
	fresh := bundleOf("autumn", 9)
	if got, want := other.artifactDirForBuild(fresh), filepath.Join(base, "event"); got != want {
		t.Errorf("a build with no previous bundle goes to %s, want %s", got, want)
	}
}

// Told where a schema's bundles belong, that is where they go, wherever the
// generation before it happens to be: giving a schema a destination it did
// not have is an operator saying its bundles move there. The copy left in
// the directory it moved out of is removed once the new one is in place, or
// an artifact server would go on publishing the older generation at the
// prefix the schema used to use.
func TestArtifactNamespaceChangeMovesTheBundle(t *testing.T) {
	m, base := namespaceManager(t)
	stale := filepath.Join(base, "7.tar.gz") // built before the schema had a destination
	if err := os.WriteFile(stale, []byte("gz"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.SetArtifactNamespaces(map[string]string{"spring": "library"}); err != nil {
		t.Fatal(err)
	}

	build := bundleOf("spring", 7)
	moved := filepath.Join(base, "library")
	if got := m.artifactDirForBuild(build); got != moved {
		t.Fatalf("a rebuild after the destination was added goes to %s, want %s", got, moved)
	}
	// As executeBuild does: promote, then prune the copy left in the
	// artifact directory itself.
	if err := os.WriteFile(filepath.Join(moved, "7.tar.gz"), []byte("gz2"), 0o644); err != nil {
		t.Fatal(err)
	}
	m.pruneStrayArtifactBundle("7.tar.gz")
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("the copy in the directory the schema moved out of survived: %v", err)
	}
	if _, err := os.Stat(filepath.Join(moved, "7.tar.gz")); err != nil {
		t.Errorf("the bundle just promoted was pruned as a stray: %v", err)
	}
}

// A schema whose destination was taken off it moves its bundles back to the
// artifact directory. "This schema belongs in the artifact directory" and
// "nothing here knows where this schema belongs" have to be different
// answers, or an operator removing a destination would have the bundles go
// on being written into the directory it named.
func TestArtifactNamespaceRemovedMovesTheBundleBack(t *testing.T) {
	m, base := namespaceManager(t)
	if err := m.SetArtifactNamespaces(map[string]string{"spring": "library"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "library", "7.tar.gz"), []byte("gz"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The same command run again, the destination now gone from the file.
	if err := m.SetArtifactNamespaces(map[string]string{"spring": ""}); err != nil {
		t.Fatal(err)
	}
	if got := m.artifactDirForBuild(bundleOf("spring", 7)); got != base {
		t.Errorf("a schema with its destination removed writes to %s, want %s", got, base)
	}
	// And a schema this command was not given keeps its bundle where it is.
	if got, want := m.artifactDirForBuild(bundleOf("autumn", 7)), filepath.Join(base, "library"); got != want {
		t.Errorf("a build of a schema this command was not given goes to %s, want %s", got, want)
	}
}

// A stray sweep takes only this build's copies. Build ids are unique to a
// plane, which is what makes removing by filename safe, and a sweep that
// took a neighbour's bundle would drop a live challenge's downloads.
func TestPruneStrayArtifactBundlesLeavesOthersAlone(t *testing.T) {
	m, base := namespaceManager(t)
	if err := m.SetArtifactNamespaces(map[string]string{"spring": "library", "autumn": "event"}); err != nil {
		t.Fatal(err)
	}
	keep := []string{filepath.Join(base, "8.tar.gz"), filepath.Join(base, "event", "9.tar.gz")}
	for _, path := range append([]string{filepath.Join(base, "7.tar.gz"), filepath.Join(base, "library", "7.tar.gz")}, keep...) {
		if err := os.WriteFile(path, []byte("gz"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	m.pruneStrayArtifactBundle("7.tar.gz")
	if _, err := os.Stat(filepath.Join(base, "7.tar.gz")); !os.IsNotExist(err) {
		t.Errorf("the stray copy of build 7 survived: %v", err)
	}
	for _, path := range append(keep, filepath.Join(base, "library", "7.tar.gz")) {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s was swept with build 7's strays: %v", path, err)
		}
	}
}

// A bundle is removed wherever it was written. remove-schema is given a name
// and never learns a destination, and a migration reads the destination a
// schema is moving TO, so neither can compute the directory -- and a bundle
// that is not found is one the build plane keeps forever while an artifact
// server goes on publishing a challenge that no longer exists.
func TestRemoveArtifactBundleFindsANamespacedOne(t *testing.T) {
	m, base := namespaceManager(t)
	if err := m.SetArtifactNamespaces(map[string]string{"spring": "library"}); err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(base, "library", "7.tar.gz")
	if err := os.WriteFile(bundle, []byte("gz"), 0o644); err != nil {
		t.Fatal(err)
	}

	// As a later command has it: this schema's namespace is unknown, so the
	// computed directory is the artifact directory and the bundle is not
	// there.
	later, _ := namespaceManager(t)
	later.artifactsDir = base
	if err := later.removeArtifactBundle("spring", "7.tar.gz"); err != nil {
		t.Fatalf("removeArtifactBundle: %s", err)
	}
	if _, err := os.Stat(bundle); !os.IsNotExist(err) {
		t.Errorf("the bundle in the namespace survived: %v", err)
	}
}

// The directory itself is looked at first, which is where a single-host
// deployment and a schema with no destination put their bundles.
func TestRemoveArtifactBundleTakesTheUnnamespacedOne(t *testing.T) {
	m, base := namespaceManager(t)
	bundle := filepath.Join(base, "9.tar.gz")
	if err := os.WriteFile(bundle, []byte("gz"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.removeArtifactBundle("spring", "9.tar.gz"); err != nil {
		t.Fatalf("removeArtifactBundle: %s", err)
	}
	if _, err := os.Stat(bundle); !os.IsNotExist(err) {
		t.Errorf("the bundle survived: %v", err)
	}
}

// A bundle that is nowhere reports not-exist, which is what destroyImages
// tolerates and logs. Anything else would fail a destroy over a file.
func TestRemoveArtifactBundleReportsAMissingOne(t *testing.T) {
	m, _ := namespaceManager(t)
	if err := m.removeArtifactBundle("spring", "404.tar.gz"); !os.IsNotExist(err) {
		t.Errorf("a bundle that is nowhere: %v, want a not-exist error", err)
	}
}

// Removing one build's bundle leaves another's alone, in the same namespace
// and in others. The search is by filename and build ids are unique to a
// plane, which is what makes it safe; a bundle removed by name alone would
// take a neighbour's files out of publication.
func TestRemoveArtifactBundleLeavesOthersAlone(t *testing.T) {
	m, base := namespaceManager(t)
	if err := m.SetArtifactNamespaces(map[string]string{"spring": "library", "autumn": "event"}); err != nil {
		t.Fatal(err)
	}
	keep := []string{
		filepath.Join(base, "library", "8.tar.gz"),
		filepath.Join(base, "event", "9.tar.gz"),
		filepath.Join(base, "10.tar.gz"),
	}
	for _, path := range append([]string{filepath.Join(base, "library", "7.tar.gz")}, keep...) {
		if err := os.WriteFile(path, []byte("gz"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.removeArtifactBundle("spring", "7.tar.gz"); err != nil {
		t.Fatalf("removeArtifactBundle: %s", err)
	}
	for _, path := range keep {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s was removed with another build's bundle: %v", path, err)
		}
	}
}

// A subdirectory cork did not make is not a namespace and is never touched,
// however much the file in it looks like a bundle. CORK_ARTIFACT_DIR is
// allowed to be the challenge tree -- it is in the ansible role's defaults --
// so the subdirectories of an artifact directory are often challenges, and a
// search that took them for namespaces would delete a challenge's own file
// for being named after a build id.
func TestArtifactSearchIgnoresDirectoriesCorkDidNotMake(t *testing.T) {
	m, base := namespaceManager(t)
	if err := m.SetArtifactNamespaces(map[string]string{"spring": "library"}); err != nil {
		t.Fatal(err)
	}
	// A challenge directory that happens to ship a numbered archive.
	challenge := filepath.Join(base, "binex101")
	if err := os.MkdirAll(challenge, 0o755); err != nil {
		t.Fatal(err)
	}
	theirs := filepath.Join(challenge, "7.tar.gz")
	if err := os.WriteFile(theirs, []byte("a challenge's own file"), 0o644); err != nil {
		t.Fatal(err)
	}

	if where, ok := m.findArtifactBundle("7.tar.gz"); ok {
		t.Errorf("a file in an unmarked subdirectory was taken for a bundle: %s", where)
	}
	// As executeBuild does: nothing is found, so nothing is pruned.
	m.pruneStrayArtifactBundle("7.tar.gz")
	if _, err := os.Stat(theirs); err != nil {
		t.Errorf("a challenge's own file was swept as a stray bundle: %v", err)
	}
	later, _ := namespaceManager(t)
	later.artifactsDir = base
	if err := later.removeArtifactBundle("spring", "7.tar.gz"); !os.IsNotExist(err) {
		t.Errorf("removeArtifactBundle reached into an unmarked subdirectory: %v", err)
	}
	if _, err := os.Stat(theirs); err != nil {
		t.Errorf("a challenge's own file was removed as a bundle: %v", err)
	}
}

// The note an orchestrator makes about an artifact directory it will never
// fill, so a unit file grown from a build plane's says so at startup rather
// than leaving its operator waiting for bundles.
func TestExternalPlaneNotesTheArtifactSettings(t *testing.T) {
	t.Setenv(ARTIFACT_DIR_ENV, filepath.Join(t.TempDir(), "artifacts"))
	t.Setenv(maxArtifactBytesEnv, "1g")

	var logged bytes.Buffer
	m := &Manager{log: captureLog(&logged), externalBuildPlane: true}
	if err := m.setDirectories(); err != nil {
		t.Fatalf("setDirectories: %s", err)
	}
	if err := m.initPolicy(); err != nil {
		t.Fatalf("initPolicy: %s", err)
	}
	for _, name := range []string{ARTIFACT_DIR_ENV, maxArtifactBytesEnv} {
		if !strings.Contains(logged.String(), name+" is set but ignored") {
			t.Errorf("no note that %s is ignored; log: %s", name, logged.String())
		}
	}
	// The one that is not set is not complained about.
	if strings.Contains(logged.String(), maxArtifactFilesEnv+" is set but ignored") {
		t.Errorf("%s was noted although it is unset; log: %s", maxArtifactFilesEnv, logged.String())
	}
}

// Another destination's bundle of the same name is left alone. A filename is
// "<build id>.tar.gz" and a build id is unique only within one plane's
// database; rebuild that database, as BUILDER.md says a plane may, and its
// ids start at 1 again while every destination's directory still holds
// bundles 1..N. A promote that swept every namespace by filename would then
// delete a different orchestrator's live bundles, and the artifact server
// would carry that deletion into the bucket.
func TestAPromoteLeavesAnotherDestinationsBundleAlone(t *testing.T) {
	m, base := namespaceManager(t)
	if err := m.SetArtifactNamespaces(map[string]string{"spring": "event", "year": "library"}); err != nil {
		t.Fatal(err)
	}
	// The library destination is serving build 1, from an earlier life of the
	// build plane's database.
	live := filepath.Join(base, "library", "1.tar.gz")
	if err := os.WriteFile(live, []byte("library's own"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A fresh plane database draws id 1 again, for a different challenge,
	// bound to the other destination.
	build := bundleOf("spring", 1)
	into := m.artifactDirForBuild(build)
	if want := filepath.Join(base, "event"); into != want {
		t.Fatalf("the new build goes to %s, want %s", into, want)
	}
	if err := os.WriteFile(filepath.Join(into, "1.tar.gz"), []byte("event's own"), 0o644); err != nil {
		t.Fatal(err)
	}
	// As executeBuild does after promoting into a namespace.
	m.pruneStrayArtifactBundle("1.tar.gz")

	if got, err := os.ReadFile(live); err != nil {
		t.Errorf("the library destination's live bundle was deleted by a build for another destination: %v", err)
	} else if string(got) != "library's own" {
		t.Errorf("the library destination's bundle is now %q", got)
	}
}

// A schema given a destination it did not have has its existing bundles
// moved. Nothing rebuilds them: a destination is not part of a build's
// identity, so the converge finds every build complete and executeBuild
// never runs -- which left the files at the old prefix while the new
// directory sat empty and every message said the deploy worked.
func TestRelocateArtifactBundlesMovesThemIn(t *testing.T) {
	m := setupTestManager(t)
	m.log = captureLog(&bytes.Buffer{})
	base := t.TempDir()
	m.artifactsDir = base
	seedBuildRows(t, m, "spring", 4, 5)
	// Built before the schema had a destination, so in the artifact
	// directory itself.
	for _, id := range []int{4, 5} {
		if err := os.WriteFile(filepath.Join(base, fmt.Sprintf("%d.tar.gz", id)), []byte("gz"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.SetArtifactNamespaces(map[string]string{"spring": "library"}); err != nil {
		t.Fatal(err)
	}

	moved, err := m.RelocateArtifactBundles("spring")
	if err != nil {
		t.Fatalf("RelocateArtifactBundles: %s", err)
	}
	if moved != 2 {
		t.Errorf("moved %d bundles, want 2", moved)
	}
	for _, id := range []int{4, 5} {
		name := fmt.Sprintf("%d.tar.gz", id)
		if _, err := os.Stat(filepath.Join(base, "library", name)); err != nil {
			t.Errorf("%s was not moved into the destination's directory: %v", name, err)
		}
		if _, err := os.Stat(filepath.Join(base, name)); !os.IsNotExist(err) {
			t.Errorf("%s is still in the artifact directory: %v", name, err)
		}
	}
}

// And it never reaches into another destination's directory. A bundle is
// named "<build id>.tar.gz" and an id is unique only within one plane's
// database, so a file of that name under another destination is not evidence
// of anything: moving it would take a live challenge's downloads away from
// the orchestrator serving them.
func TestRelocateArtifactBundlesLeavesOtherDestinationsAlone(t *testing.T) {
	m := setupTestManager(t)
	m.log = captureLog(&bytes.Buffer{})
	base := t.TempDir()
	m.artifactsDir = base
	seedBuildRows(t, m, "spring", 1)
	if err := m.SetArtifactNamespaces(map[string]string{"spring": "event", "year": "library"}); err != nil {
		t.Fatal(err)
	}
	live := filepath.Join(base, "library", "1.tar.gz")
	if err := os.WriteFile(live, []byte("library's own"), 0o644); err != nil {
		t.Fatal(err)
	}

	moved, err := m.RelocateArtifactBundles("spring")
	if err != nil {
		t.Fatalf("RelocateArtifactBundles: %s", err)
	}
	if moved != 0 {
		t.Errorf("moved %d bundles, want none: nothing of this schema's was in the artifact directory", moved)
	}
	if got, err := os.ReadFile(live); err != nil || string(got) != "library's own" {
		t.Errorf("another destination's bundle was moved: %v %q", err, got)
	}
}

// seedBuildRows puts build rows on record under the given ids, which is what
// RelocateArtifactBundles reads to know which bundles are a schema's.
func seedBuildRows(t *testing.T, m *Manager, schema string, ids ...int) {
	t.Helper()
	for _, id := range ids {
		_, err := m.db.Exec(
			`INSERT INTO challenges (id, name, namespace, challengetype, description, details, sourcechecksum, metadatachecksum, path, solvescript, templatable, maxusers, points)
			 VALUES (?, 'x', '', 'custom', '', '', 0, 0, '', 0, 0, 0, 0) ON CONFLICT DO NOTHING;`,
			"test/"+schema)
		if err != nil {
			t.Fatalf("seeding a challenge: %s", err)
		}
		_, err = m.db.Exec(
			`INSERT INTO builds (id, flag, seed, format, checksum, hasartifacts, challenge, schema, instancecount)
			 VALUES (?, 'flag{x}', ?, 'flag{%s}', 1, 1, ?, ?, 0);`,
			id, id, "test/"+schema, schema)
		if err != nil {
			t.Fatalf("seeding build %d: %s", id, err)
		}
	}
}

// The bundle arrives at its destination without a served name ever being
// renamed out of the directory it left.
//
// What publishes these bundles watches the artifact directory and each
// namespace under it, and a rename between two watched directories arrives
// as one event naming the source path -- a path that no longer exists. A
// watcher that decides what a path is from the path alone then opens a file
// that is gone. This asserts the shape that avoids it: the destination file
// appears by a rename from a dot-prefixed name WITHIN the destination
// directory, and the source is unlinked rather than renamed away.
func TestRelocateDoesNotRenameAServedNameAcross(t *testing.T) {
	m := setupTestManager(t)
	m.log = captureLog(&bytes.Buffer{})
	base := t.TempDir()
	m.artifactsDir = base
	seedBuildRows(t, m, "spring", 4)
	if err := m.SetArtifactNamespaces(map[string]string{"spring": "library"}); err != nil {
		t.Fatal(err)
	}
	from := filepath.Join(base, "4.tar.gz")
	if err := os.WriteFile(from, []byte("the bundle"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A hard link to the source. os.Rename moves the directory entry, so a
	// renamed file keeps its inode and the link count stays 2; a copy makes
	// a new inode, so the destination is not the same file the link names.
	witness := filepath.Join(base, "witness.keep")
	if err := os.Link(from, witness); err != nil {
		t.Skipf("hard links unavailable here: %s", err)
	}

	moved, err := m.RelocateArtifactBundles("spring")
	if err != nil {
		t.Fatalf("RelocateArtifactBundles: %s", err)
	}
	if moved != 1 {
		t.Fatalf("moved %d bundles, want 1", moved)
	}

	to := filepath.Join(base, "library", "4.tar.gz")
	got, err := os.ReadFile(to)
	if err != nil {
		t.Fatalf("the bundle is not at its destination: %s", err)
	}
	if string(got) != "the bundle" {
		t.Errorf("the relocated bundle holds %q", got)
	}
	if _, err := os.Stat(from); !os.IsNotExist(err) {
		t.Errorf("the source bundle survived the move: %v", err)
	}
	if !os.SameFile(mustStat(t, witness), mustStat(t, witness)) {
		t.Fatal("sanity: a file is not itself")
	}
	// The destination must NOT be the source's inode: if it were, the move
	// was a cross-directory rename and a watcher would have been handed the
	// vanished source path.
	if os.SameFile(mustStat(t, witness), mustStat(t, to)) {
		t.Error("the bundle was renamed across directories rather than copied: a watcher on the artifact directory is handed the source path, which no longer exists")
	}
	// Exactly the bundle and the namespace marker: nothing staged left
	// behind, and nothing else invented. Asserted as the whole directory
	// listing rather than as "no .relocating file", which passes just as
	// happily when there was never any staging at all.
	entries, err := os.ReadDir(filepath.Join(base, "library"))
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	slices.Sort(names)
	want := []string{artifactNamespaceMarker, "4.tar.gz"}
	slices.Sort(want)
	if !slices.Equal(names, want) {
		t.Errorf("the destination holds %v, want %v", names, want)
	}
}

// The staging step itself: a bundle only ever appears under its served name
// by a rename within its own directory, so nothing can read a half-written
// file under a name it would publish. Asserted by making the destination
// read-only after the copy would have started, which leaves the staged file
// undone and must leave no partial "<id>.tar.gz" behind.
func TestRelocateLeavesNoPartialUnderAServedName(t *testing.T) {
	m := setupTestManager(t)
	m.log = captureLog(&bytes.Buffer{})
	base := t.TempDir()
	m.artifactsDir = base
	seedBuildRows(t, m, "spring", 4)
	if err := m.SetArtifactNamespaces(map[string]string{"spring": "library"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "4.tar.gz"), []byte("the bundle"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A directory entry in the way of the staging name, which no file
	// operation can overwrite: the copy fails before anything is renamed.
	staged := filepath.Join(base, "library", ".4.tar.gz.relocating")
	if err := os.Mkdir(staged, 0o755); err != nil {
		t.Fatal(err)
	}

	moved, err := m.RelocateArtifactBundles("spring")
	if err == nil {
		t.Fatal("a relocation that could not stage its copy reported success")
	}
	if moved != 0 {
		t.Errorf("reported %d moved", moved)
	}
	if _, err := os.Stat(filepath.Join(base, "library", "4.tar.gz")); !os.IsNotExist(err) {
		t.Errorf("a bundle appeared under its served name although the copy failed: %v", err)
	}
	// And the original is still there, so nothing was lost.
	if content, err := os.ReadFile(filepath.Join(base, "4.tar.gz")); err != nil || string(content) != "the bundle" {
		t.Errorf("the source bundle was disturbed by a failed relocation: %v %q", err, content)
	}
}

func mustStat(t *testing.T, path string) os.FileInfo {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %s", path, err)
	}
	return info
}
