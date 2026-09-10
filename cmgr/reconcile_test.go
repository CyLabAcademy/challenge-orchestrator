package cmgr

import "testing"

// reconcileBuild is the half of a rebuild after the image build: it rotates
// the retention pair, finalizes the row, and reports the generation that
// leaves retention. With no instances and no registry it is database only,
// which is what a hand-over on an external build plane needs of it too.
func TestReconcileBuildRotatesRetention(t *testing.T) {
	m := setupTestManager(t)
	defer m.db.Close()

	challenge := testChallenge("test/reconcile", 0x5555)
	if errs := m.addChallenges([]*ChallengeMetadata{challenge}); len(errs) > 0 {
		t.Fatalf("addChallenges: %v", errs)
	}
	id := insertTestBuild(t, m, "event", string(challenge.Id), "flag{%s}", 1, 0x1111)
	// On demand: nothing to converge to, so no launch is attempted.
	if _, err := m.db.Exec("UPDATE builds SET prevchecksum = ?, instancecount = ? WHERE id = ?;", 0x1010, DYNAMIC_INSTANCES, id); err != nil {
		t.Fatal(err)
	}
	cMeta, err := m.lookupChallengeMetadata(challenge.Id)
	if err != nil {
		t.Fatal(err)
	}
	build, err := m.lookupBuildMetadata(id)
	if err != nil {
		t.Fatal(err)
	}

	// The new generation, as a build or a hand-over leaves it.
	build.Flag = "flag{new}"
	build.Checksum = 0x2222
	build.SourceChecksum = 0x5555
	build.Images = []Image{{Host: "challenge", Ports: []string{"1337/tcp"}}}

	candidate, errs := m.reconcileBuild(build, cMeta, map[string]string{}, true)
	if len(errs) > 0 {
		t.Fatalf("reconcileBuild: %v", errs)
	}
	stored, err := m.lookupBuildMetadata(id)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Flag != "flag{new}" || stored.Checksum != 0x2222 || stored.SourceChecksum != 0x5555 {
		t.Fatalf("row not finalized: %+v", stored)
	}
	if stored.PrevChecksum != 0x1111 {
		t.Fatalf("rollback target = %x, want the replaced generation 1111", stored.PrevChecksum)
	}
	if candidate == nil {
		t.Fatal("the displaced generation was not reported for pruning")
	}
	if candidate.meta.Checksum != 0x1010 || len(candidate.tags) != 1 {
		t.Fatalf("prune candidate = %+v, want generation 1010 over one image", candidate)
	}

	// Without --prune-old nothing is reported; a rebuild to the same
	// content leaves the rollback target untouched.
	build.Checksum = 0x2222
	candidate, errs = m.reconcileBuild(build, cMeta, map[string]string{}, false)
	if len(errs) > 0 || candidate != nil {
		t.Fatalf("second reconcile: candidate %v, errors %v", candidate, errs)
	}
	stored, _ = m.lookupBuildMetadata(id)
	if stored.PrevChecksum != 0x1111 {
		t.Fatalf("an unchanged generation rotated the rollback target to %x", stored.PrevChecksum)
	}

	// A row opened but never finalized serves no generation: the checksum
	// openBuild stamps on it is no rollback target, and nothing leaves
	// retention. That is the row a hand-over of a new build lands on.
	opened := insertTestBuildRow(t, m, "", "event", string(challenge.Id), "flag{%s}", 2, 0xABCD)
	if _, err := m.db.Exec("UPDATE builds SET instancecount = ? WHERE id = ?;", DYNAMIC_INSTANCES, opened); err != nil {
		t.Fatal(err)
	}
	build, err = m.lookupBuildMetadata(opened)
	if err != nil {
		t.Fatal(err)
	}
	build.Flag = "flag{first}"
	build.Checksum = 0x3333
	build.SourceChecksum = 0x5555
	build.Images = []Image{{Host: "challenge", Ports: []string{"1337/tcp"}}}
	candidate, errs = m.reconcileBuild(build, cMeta, map[string]string{}, true)
	if len(errs) > 0 || candidate != nil {
		t.Fatalf("first generation: candidate %v, errors %v", candidate, errs)
	}
	stored, _ = m.lookupBuildMetadata(opened)
	if stored.Flag != "flag{first}" || stored.Checksum != 0x3333 || stored.PrevChecksum != 0 {
		t.Fatalf("a first generation left the row at %+v, want no rollback target", stored)
	}
}
