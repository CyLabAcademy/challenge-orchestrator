package cmgr

import (
	"errors"
	"strings"
	"testing"
)

// On an external build plane a converge is refused before it locks or
// releases anything, naming every challenge and build the hand-over has
// not delivered; once everything is there it converges as before.
func TestConvergeSchemaExternalRefusesBeforeLocking(t *testing.T) {
	m := setupExternalTestManager(t)
	present := testChallenge("test/handed-over", 0x1111)
	if errs := m.addChallenges([]*ChallengeMetadata{present}); len(errs) > 0 {
		t.Fatalf("addChallenges: %v", errs)
	}
	built := insertTestBuild(t, m, "event", string(present.Id), "flag{%s}", 1, 0x1111)
	insertTestBuildRow(t, m, "", "event", string(present.Id), "flag{%s}", 2, 0x1112)

	schema := &Schema{Name: "event", FlagFormat: "flag{%s}", Challenges: map[ChallengeId]BuildSpecification{
		present.Id:               {Seeds: []int{1, 2}, InstanceCount: DYNAMIC_INSTANCES},
		"test/never-handed-over": {Seeds: []int{1}, InstanceCount: DYNAMIC_INSTANCES},
		// Wants nothing, so nothing is required of it, as on a local plane.
		"test/no-seeds": {InstanceCount: 1},
	}}

	errs := m.convergeSchema(schema)
	if len(errs) != 2 {
		t.Fatalf("got %d errors, want one per shortfall: %v", len(errs), errs)
	}
	for _, err := range errs {
		if !errors.Is(err, ErrExternalBuildPlane) {
			t.Errorf("not the external refusal: %v", err)
		}
	}
	joined := errors.Join(errs...).Error()
	for _, want := range []string{"'test/never-handed-over' has not been handed over", "'test/handed-over'", "seed 2"} {
		if !strings.Contains(joined, want) {
			t.Errorf("refusal does not name %q: %s", want, joined)
		}
	}
	if strings.Contains(joined, "seed 1") {
		t.Errorf("refusal names the build that was handed over: %s", joined)
	}
	if strings.Contains(joined, "no-seeds") {
		t.Errorf("refusal names an entry that wants no build: %s", joined)
	}

	// Nothing was locked, written or released.
	row, err := m.lookupBuildMetadata(built)
	if err != nil {
		t.Fatal(err)
	}
	if row.InstanceCount == LOCKED {
		t.Fatal("the refused converge locked the schema's builds")
	}
	var count int
	if err := m.db.Get(&count, "SELECT COUNT(1) FROM builds;"); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("the refused converge left %d build rows, want the 2 it found", count)
	}

	// With every build handed over, the converge goes through: the row's
	// instance count follows the schema, and nothing is built.
	if _, err := m.db.Exec("UPDATE builds SET flag = 'flag{y}' WHERE challenge = ? AND seed = 2;", present.Id); err != nil {
		t.Fatal(err)
	}
	delete(schema.Challenges, "test/never-handed-over")
	if errs := m.convergeSchema(schema); len(errs) > 0 {
		t.Fatalf("a converge over handed-over builds failed: %v", errs)
	}
	if row, err = m.lookupBuildMetadata(built); err != nil {
		t.Fatal(err)
	}
	if row.InstanceCount != DYNAMIC_INSTANCES {
		t.Fatalf("instance count = %d after the converge, want %d", row.InstanceCount, DYNAMIC_INSTANCES)
	}
}

// The precondition is the external build plane's alone: a local converge
// of the same schema goes on to open the build, and fails there on the
// row the challenge does not have, never on a hand-over it does not need.
func TestConvergeSchemaLocalNeverAsksForHandOver(t *testing.T) {
	m := setupTestManager(t)
	defer m.db.Close()
	schema := &Schema{Name: "event", FlagFormat: "flag{%s}", Challenges: map[ChallengeId]BuildSpecification{
		"test/absent": {Seeds: []int{1}, InstanceCount: 1},
	}}
	errs := m.convergeSchema(schema)
	if len(errs) == 0 {
		t.Fatal("a local converge naming a challenge with no row succeeded")
	}
	for _, err := range errs {
		if errors.Is(err, ErrExternalBuildPlane) {
			t.Errorf("a local converge gave the external build plane's refusal: %v", err)
		}
	}
}
