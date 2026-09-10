package cmgr

import "testing"

// TestConvergeBuildInstancesNothingToDo covers the database-only arm of the
// per-build instance converge: a build with no instances and a target of
// zero converges without touching docker and reports nothing.
func TestConvergeBuildInstancesNothingToDo(t *testing.T) {
	mgr := setupTestManager(t)
	defer mgr.db.Close()

	challenge := &ChallengeMetadata{
		Id:            "test/converge-none",
		Name:          "Converge None",
		Namespace:     "test",
		ChallengeType: "custom",
		Hosts:         []HostInfo{{Name: "challenge", Target: ""}},
		PortMap:       map[string]PortInfo{},
		Tags:          []string{},
		Attributes:    map[string]string{},
		Path:          "/tmp/test/problem.md",
		ChallengeOptions: ChallengeOptions{
			Overrides: map[string]ContainerOptions{"": {}},
		},
	}
	if errs := mgr.addChallenges([]*ChallengeMetadata{challenge}); len(errs) > 0 {
		t.Fatalf("addChallenges failed: %v", errs)
	}
	id := insertTestBuild(t, mgr, "schema-a", string(challenge.Id), "flag{%s}", 1, 0x1111)
	build, err := mgr.lookupBuildMetadata(id)
	if err != nil {
		t.Fatalf("lookupBuildMetadata: %s", err)
	}

	if errs := mgr.convergeBuildInstances(build, challenge, 0); len(errs) > 0 {
		t.Errorf("converging a build with no instances to zero reported errors: %v", errs)
	}
}
