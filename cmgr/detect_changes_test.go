package cmgr

import (
	"os"
	"path/filepath"
	"testing"
)

// A loadable flag-only challenge: built-in type, so no Dockerfile is needed.
const detectChangesProblem = `# Layer Cake

- Namespace: test
- Type: flag-only
- Category: General Skills
- Points: 10

## Description

Which layer of the OSI model does TCP belong to?

## Tags

- example
`

func idsOf(list []*ChallengeMetadata) []ChallengeId {
	ids := make([]ChallengeId, len(list))
	for i, md := range list {
		ids[i] = md.Id
	}
	return ids
}

func buckets(cu *ChallengeUpdates) map[string][]ChallengeId {
	return map[string][]ChallengeId{
		"Added":      idsOf(cu.Added),
		"Refreshed":  idsOf(cu.Refreshed),
		"Updated":    idsOf(cu.Updated),
		"Stale":      idsOf(cu.Stale),
		"Removed":    idsOf(cu.Removed),
		"Unmodified": idsOf(cu.Unmodified),
	}
}

// expectOnly asserts that the challenge appears in exactly the named bucket.
func expectOnly(t *testing.T, cu *ChallengeUpdates, id ChallengeId, want string) {
	t.Helper()
	if len(cu.Errors) > 0 {
		t.Fatalf("DetectChanges reported errors: %v", cu.Errors)
	}
	for name, ids := range buckets(cu) {
		found := false
		for _, got := range ids {
			if got == id {
				found = true
			}
		}
		if found != (name == want) {
			t.Errorf("expected %s only under %s; found under %s: %v", id, want, name, found)
		}
	}
}

// TestDetectChangesStale covers the Stale verdict: a challenge whose source on
// disk matches the database but whose build was produced from another source
// generation (its last rebuild failed) is reported as Stale rather than
// Unmodified, Updated still outranks Stale, and unbuilt rows are never stale.
func TestDetectChangesStale(t *testing.T) {
	mgr := setupTestManager(t)
	defer mgr.db.Close()

	root := t.TempDir()
	mgr.chalDir = root
	chalDir := filepath.Join(root, "cat", "layer-cake")
	if err := os.MkdirAll(chalDir, 0o755); err != nil {
		t.Fatalf("mkdir: %s", err)
	}
	if err := os.WriteFile(filepath.Join(chalDir, "problem.md"), []byte(detectChangesProblem), 0o644); err != nil {
		t.Fatalf("write problem.md: %s", err)
	}
	const id = ChallengeId("test/layer-cake")

	cu := mgr.DetectChanges(root)
	expectOnly(t, cu, id, "Added")
	if errs := mgr.addChallenges(cu.Added); len(errs) > 0 {
		t.Fatalf("addChallenges failed: %v", errs)
	}
	current := cu.Added[0].SourceChecksum
	if current == 0 {
		t.Fatal("expected a non-zero source checksum for the challenge on disk")
	}

	// No builds at all: nothing can be stale.
	expectOnly(t, mgr.DetectChanges(root), id, "Unmodified")

	// A build produced from the current generation, and an unbuilt row.
	built := insertTestBuild(t, mgr, "schema-a", string(id), "flag{%s}", 1, 0x1111)
	if _, err := mgr.db.Exec("UPDATE builds SET sourcechecksum = ? WHERE id = ?;", current, built); err != nil {
		t.Fatalf("stamp build: %s", err)
	}
	if _, err := mgr.db.Exec(
		`INSERT INTO builds(flag, format, seed, checksum, hasartifacts, lastsolved, challenge, schema, instancecount)
		 VALUES ('', 'flag{%s}', 2, 0x2222, 0, 0, ?, 'schema-a', 1);`, id); err != nil {
		t.Fatalf("insert unbuilt row: %s", err)
	}
	expectOnly(t, mgr.DetectChanges(root), id, "Unmodified")

	// The build's last rebuild failed: the row (and the tree) moved on, the
	// build still records the generation it serves.
	if _, err := mgr.db.Exec("UPDATE builds SET sourcechecksum = ? WHERE id = ?;", current^0xffffffff, built); err != nil {
		t.Fatalf("age build: %s", err)
	}
	cu = mgr.DetectChanges(root)
	expectOnly(t, cu, id, "Stale")
	if cu.Stale[0].SourceChecksum != current {
		t.Errorf("Stale carries the on-disk metadata: expected source checksum %#x, got %#x", current, cu.Stale[0].SourceChecksum)
	}
	ids, err := mgr.staleBuildIds(cu.Stale[0])
	if err != nil {
		t.Fatalf("staleBuildIds: %s", err)
	}
	if len(ids) != 1 || ids[0] != built {
		t.Errorf("expected only build %d to be stale, got %v", built, ids)
	}

	// The source changes as well: Updated outranks Stale (every build is
	// rebuilt by the update path anyway).
	if err := os.WriteFile(filepath.Join(chalDir, "extra.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatalf("write extra.txt: %s", err)
	}
	expectOnly(t, mgr.DetectChanges(root), id, "Updated")
}
