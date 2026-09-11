package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/CyLabAcademy/challenge-orchestrator/cmgr"
)

func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const eventSchema = `
name: event
flag_format: flag{%s}
challenges:
  test/persistent:
    seeds: [1, 2]
    instance_count: 2
  test/on-demand:
    seeds: [3]
    instance_count: -1
`

// A schema file decides what this plane builds, so what it does not say is
// worth refusing rather than defaulting.
func TestLoadSchema(t *testing.T) {
	path := writeFile(t, "event.yaml", eventSchema)
	schema, err := loadSchema(path)
	if err != nil {
		t.Fatalf("loadSchema: %s", err)
	}
	if schema.Name != "event" || schema.FlagFormat != "flag{%s}" || len(schema.Challenges) != 2 {
		t.Fatalf("schema read as %+v", schema)
	}
	spec := schema.Challenges["test/persistent"]
	if len(spec.Seeds) != 2 || spec.Seeds[0] != 1 || spec.InstanceCount != 2 {
		t.Errorf("challenge read as %+v", spec)
	}

	for _, tc := range []struct{ name, content, says string }{
		{"no-name.yaml", "flag_format: flag{%s}\nchallenges:\n  a/b:\n    seeds: [1]\n", "no name"},
		{"no-format.yaml", "name: event\nchallenges:\n  a/b:\n    seeds: [1]\n", "no flag format"},
		{"no-challenges.yaml", "name: event\nflag_format: flag{%s}\n", "names no challenges"},
		{"no-seeds.yaml", "name: event\nflag_format: flag{%s}\nchallenges:\n  a/b:\n    instance_count: 1\n  c/d:\n    seeds: [1]\n  e/f:\n    instance_count: 2\n", "no seeds for 'a/b', 'e/f'"},
		{"event.txt", eventSchema, "unrecognized schema file extension"},
	} {
		if _, err := loadSchema(writeFile(t, tc.name, tc.content)); err == nil || !strings.Contains(err.Error(), tc.says) {
			t.Errorf("%s: %v, want a refusal saying %q", tc.name, err, tc.says)
		}
	}
	if _, err := loadSchema(filepath.Join(t.TempDir(), "absent.yaml")); err == nil {
		t.Error("a schema file that is not there was read")
	}
}

// A build carries the identity it was made under, and the orchestrator
// recomputes that from what the hand-over states and refuses what
// disagrees. A build made under base image pins that have since moved is
// therefore caught here, where the answer is legible, rather than there.
func TestIdentityMismatches(t *testing.T) {
	first := builtBuild("event", 1, 1)
	first.Checksum, first.SourceChecksum = 0xAAAA, 0x1111
	second := builtBuild("event", 2, 1)
	second.Checksum, second.SourceChecksum = 0xBBBB, 0x1111
	challenge := builtChallenge("test/pinned", 0x1111, first, second)
	payloads := map[cmgr.ChallengeId]*cmgr.HandOver{challenge.Id: {Challenge: challenge}}
	order := []cmgr.ChallengeId{challenge.Id}

	// The identity is asked for with the challenge's own type, the
	// template being one of its inputs.
	types := map[string]bool{}
	agreeing := func(build *cmgr.BuildMetadata, challengeType string) uint32 {
		types[challengeType] = true
		return build.Checksum
	}
	if got := identityMismatches(payloads, order, agreeing); len(got) != 0 {
		t.Fatalf("builds that agree with their inputs: %v", got)
	}
	if len(types) != 1 || !types["custom"] {
		t.Errorf("the identity was asked for with types %v, want the challenge's own", types)
	}

	// The pins moved under one of them.
	moved := func(build *cmgr.BuildMetadata, challengeType string) uint32 {
		if build.Seed == 2 {
			return 0xCCCC
		}
		return build.Checksum
	}
	got := identityMismatches(payloads, order, moved)
	if len(got) != 1 {
		t.Fatalf("%d complaint(s), want the one build that does not agree: %v", len(got), got)
	}
	for _, want := range []string{"test/pinned", "seed 2", "bbbb", "cccc", "base image pins"} {
		if !strings.Contains(got[0], want) {
			t.Errorf("the complaint does not name %q: %s", want, got[0])
		}
	}

	// The other way a build is not what it would be handed over as: its
	// rebuild failed, so it stayed at the generation it serves while the
	// challenge moved on. Its content identity agrees with its own source
	// generation, so only this catches it.
	stale := builtBuild("event", 3, 1)
	stale.Checksum, stale.SourceChecksum = 0xDDDD, 0x9999
	challenge.Builds = append(challenge.Builds, stale)
	got = identityMismatches(payloads, order, agreeing)
	if len(got) != 1 {
		t.Fatalf("%d complaint(s), want the one build left behind: %v", len(got), got)
	}
	for _, want := range []string{"seed 3", "source generation 9999", "1111"} {
		if !strings.Contains(got[0], want) {
			t.Errorf("the complaint does not name %q: %s", want, got[0])
		}
	}
}

// This plane builds a schema and runs none of it: every build is converged
// on demand, which launches nothing, whatever the schema asks for.
func TestForBuilding(t *testing.T) {
	schema, err := loadSchema(writeFile(t, "event.yaml", eventSchema))
	if err != nil {
		t.Fatal(err)
	}
	building := forBuilding(schema)
	if building.Name != schema.Name || building.FlagFormat != schema.FlagFormat {
		t.Fatalf("the schema built is not the schema given: %+v", building)
	}
	for id, spec := range building.Challenges {
		if spec.InstanceCount != cmgr.DYNAMIC_INSTANCES {
			t.Errorf("'%s' is converged at instance count %d here, which launches", id, spec.InstanceCount)
		}
		if len(spec.Seeds) != len(schema.Challenges[id].Seeds) {
			t.Errorf("'%s' lost its seeds: %v", id, spec.Seeds)
		}
	}
	// The schema given is untouched: its counts are what the hand-over
	// carries.
	if schema.Challenges["test/persistent"].InstanceCount != 2 {
		t.Error("forBuilding changed the schema it was given")
	}
}

func builtChallenge(id cmgr.ChallengeId, source uint32, builds ...*cmgr.BuildMetadata) *cmgr.ChallengeMetadata {
	return &cmgr.ChallengeMetadata{
		Id:             id,
		ChallengeType:  "custom",
		SourceChecksum: source,
		Hosts:          []cmgr.HostInfo{{Name: "challenge"}},
		Builds:         builds,
	}
}

func builtBuild(schema string, seed int, count int) *cmgr.BuildMetadata {
	return &cmgr.BuildMetadata{
		Id: cmgr.BuildId(seed), Schema: schema, Format: "flag{%s}", Seed: seed,
		Flag: "flag{x}", InstanceCount: count,
		Images:    []cmgr.Image{{Host: "challenge"}},
		Instances: []*cmgr.InstanceMetadata{{Id: 7}},
	}
}

// The payload is what the orchestrator is to record: the challenge as this
// plane scanned it, every build of it any schema names, each at the count
// its own schema asks for rather than the on-demand one it was built at,
// and nothing of what runs here.
func TestAssemble(t *testing.T) {
	event := &cmgr.Schema{Name: "event", FlagFormat: "flag{%s}", Challenges: map[cmgr.ChallengeId]cmgr.BuildSpecification{
		"test/shared": {Seeds: []int{1}, InstanceCount: 2},
		"test/only":   {Seeds: []int{4}, InstanceCount: cmgr.DYNAMIC_INSTANCES},
	}}
	practice := &cmgr.Schema{Name: "practice", FlagFormat: "flag{%s}", Challenges: map[cmgr.ChallengeId]cmgr.BuildSpecification{
		"test/shared": {Seeds: []int{9}, InstanceCount: 1},
	}}
	built := map[string][]*cmgr.ChallengeMetadata{
		"event": {
			builtChallenge("test/shared", 0x1111, builtBuild("event", 1, cmgr.DYNAMIC_INSTANCES)),
			builtChallenge("test/only", 0x2222, builtBuild("event", 4, cmgr.DYNAMIC_INSTANCES)),
		},
		"practice": {
			builtChallenge("test/shared", 0x1111, builtBuild("practice", 9, cmgr.DYNAMIC_INSTANCES)),
		},
	}

	payloads, order := assemble([]*cmgr.Schema{event, practice}, built, 0xABCD)
	if len(order) != 2 || order[0] != "test/shared" || order[1] != "test/only" {
		t.Fatalf("challenges handed over in the order %v", order)
	}
	shared := payloads["test/shared"]
	if shared.PinFingerprint != 0xABCD {
		t.Errorf("pin fingerprint %x, want the one the builds were made under", shared.PinFingerprint)
	}
	if shared.Challenge.SourceChecksum != 0x1111 {
		t.Errorf("challenge recorded as %+v", shared.Challenge)
	}
	// A challenge two schemas name arrives once, with the builds of both.
	if len(shared.Challenge.Builds) != 2 {
		t.Fatalf("'test/shared' carries %d build(s), want one per schema", len(shared.Challenge.Builds))
	}
	for _, build := range shared.Challenge.Builds {
		want := 2
		if build.Schema == "practice" {
			want = 1
		}
		if build.InstanceCount != want {
			t.Errorf("build of schema '%s' handed over at count %d, want the schema's %d", build.Schema, build.InstanceCount, want)
		}
		if len(build.Instances) != 0 {
			t.Errorf("build of schema '%s' carries this plane's instances: %+v", build.Schema, build.Instances)
		}
	}
	only := payloads["test/only"]
	if len(only.Challenge.Builds) != 1 || only.Challenge.Builds[0].InstanceCount != cmgr.DYNAMIC_INSTANCES {
		t.Errorf("'test/only' carries %+v", only.Challenge.Builds)
	}
}
