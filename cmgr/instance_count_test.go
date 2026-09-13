package cmgr

import (
	"strings"
	"testing"
)

// A schema's instance_count says what should be running, and three of its
// values carry weight in different ways: -1 is on demand, a positive count
// is a fixed set the converge maintains, and 0 is deployed-but-closed --
// images in the registry, nothing running, every launch refused.
//
// That last one is the documented way to take a challenge out of service
// without destroying it (BUILDER.md), and the whole of it rests on Start
// refusing anything that is not DYNAMIC_INSTANCES. Worth pinning, because
// nothing else tests it and the reading is easy to get backwards: 0 does
// not mean "no limit", it means a build nobody can launch.
func TestStartRefusesAnythingButOnDemand(t *testing.T) {
	m := setupExternalTestManager(t)
	challenge := testChallenge("test/closed", 0)
	if errs := m.addChallenges([]*ChallengeMetadata{challenge}); len(errs) > 0 {
		t.Fatalf("addChallenges: %v", errs)
	}

	for _, tc := range []struct {
		name    string
		count   int
		locked  bool
		because string
	}{
		{"on demand", DYNAMIC_INSTANCES, false, "-1 is the one value a launch is allowed for"},
		{"closed", 0, true, "0 is deployed and closed: the converge runs none and nobody may start one"},
		{"a fixed count", 3, true, "the schema decides how many run, so a launch beside them is refused"},
		{"locked", LOCKED, true, "a removed schema's builds are on their way out"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A seed of its own: a build row is unique per schema, format,
			// challenge and seed.
			id := insertTestBuild(t, m, "event", string(challenge.Id), "flag{%s}", tc.count+10, 0x2222)
			if _, err := m.db.Exec("UPDATE builds SET instancecount = ? WHERE id = ?;", tc.count, id); err != nil {
				t.Fatal(err)
			}

			// The refusal is read off the message rather than the absence of
			// one: an on-demand build gets past this guard and then fails
			// for want of a worker, which is a different answer and the
			// point of the distinction.
			_, err := m.Start(id, nil)
			lockedOut := err != nil && strings.Contains(err.Error(), "locked build")
			if lockedOut != tc.locked {
				t.Errorf("instance_count %d: locked out = %v, want %v (%s); Start said: %v",
					tc.count, lockedOut, tc.locked, tc.because, err)
			}
		})
	}
}
