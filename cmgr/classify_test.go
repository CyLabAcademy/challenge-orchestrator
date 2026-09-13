package cmgr

import "testing"

// classifyChallenges is DetectChanges without the scan: given an inventory
// and the rows, it sorts them the way the directory scan always has, and
// the hand-over will. The rows come from the database so that safeToRefresh
// has its persisted options to compare against.
func TestClassifyChallenges(t *testing.T) {
	m := setupTestManager(t)
	defer m.db.Close()

	kept := testChallenge("test/classify-kept", 0x1000)
	changed := testChallenge("test/classify-changed", 0x2000)
	touched := testChallenge("test/classify-touched", 0x3000)
	gone := testChallenge("test/classify-gone", 0x4000)
	stale := testChallenge("test/classify-stale", 0x5000)
	for _, c := range []*ChallengeMetadata{kept, changed, touched, gone, stale} {
		c.MetadataChecksum = 0x10
	}
	if errs := m.addChallenges([]*ChallengeMetadata{kept, changed, touched, gone, stale}); len(errs) > 0 {
		t.Fatalf("addChallenges: %v", errs)
	}
	current, err := m.listChallenges()
	if err != nil {
		t.Fatal(err)
	}

	// The inventory: kept as it is, changed in source, touched in metadata
	// only, a newcomer, and stale unchanged; gone is absent.
	inventory := map[ChallengeId]*ChallengeMetadata{}
	for _, c := range []*ChallengeMetadata{kept, changed, touched, stale} {
		copy := *c
		inventory[c.Id] = &copy
	}
	inventory[changed.Id].SourceChecksum = 0x2001
	inventory[touched.Id].MetadataChecksum = 0x11
	added := testChallenge("test/classify-added", 0x6000)
	inventory[added.Id] = added

	removedAsked := map[ChallengeId]bool{}
	cu := m.classifyChallenges(inventory, current, map[ChallengeId]bool{stale.Id: true}, func(row *ChallengeMetadata) bool {
		removedAsked[row.Id] = true
		return row.Id == gone.Id
	})

	for id, want := range map[ChallengeId]string{
		kept.Id:    "Unmodified",
		changed.Id: "Updated",
		touched.Id: "Refreshed",
		added.Id:   "Added",
		gone.Id:    "Removed",
		stale.Id:   "Stale",
	} {
		expectOnly(t, cu, id, want)
	}
	if len(removedAsked) != 1 || !removedAsked[gone.Id] {
		t.Errorf("the removal predicate was asked about %v, want only the absent row", removedAsked)
	}
	if len(inventory) != 5 {
		t.Errorf("the inventory was modified: %d entries left", len(inventory))
	}

	// A partial inventory removes nothing unless the predicate says so: a
	// row it lacks is left alone.
	cu = m.classifyChallenges(map[ChallengeId]*ChallengeMetadata{kept.Id: inventory[kept.Id]}, current, nil, func(*ChallengeMetadata) bool { return false })
	expectOnly(t, cu, kept.Id, "Unmodified")
	if len(cu.Removed) != 0 || len(cu.Added) != 0 {
		t.Errorf("a one-challenge inventory removed %d and added %d", len(cu.Removed), len(cu.Added))
	}

	// A hand-over asks for one verdict rather than a bucket, and a
	// challenge no row exists for yet is Added.
	if verdict := m.classifyChallenge(nil, added, false); verdict != verdictAdded {
		t.Errorf("a challenge with no row got verdict %d, want Added", verdict)
	}
}
