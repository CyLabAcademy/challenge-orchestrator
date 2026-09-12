package main

import (
	"strings"
	"testing"

	"github.com/CyLabAcademy/challenge-orchestrator/cmgr"
)

func routedSchema(name, destination, format string, challenges map[cmgr.ChallengeId][]int) *cmgr.Schema {
	s := &cmgr.Schema{
		Name:        name,
		Destination: destination,
		FlagFormat:  format,
		Challenges:  map[cmgr.ChallengeId]cmgr.BuildSpecification{},
	}
	for id, seeds := range challenges {
		s.Challenges[id] = cmgr.BuildSpecification{Seeds: seeds, InstanceCount: cmgr.DYNAMIC_INSTANCES}
	}
	return s
}

// Schemas are grouped by the orchestrator that serves them before anything
// is handed over. It matters because assemble merges a challenge two schemas
// name into one payload: right when one orchestrator serves both, and wrong
// when they are served by different ones.
func TestRouteSchemas(t *testing.T) {
	dests, err := loadDestinations(writeDestinations(t, "library: http://library:4200\nevent: http://event:4200\n"))
	if err != nil {
		t.Fatal(err)
	}
	year := routedSchema("year-round", "library", "flag{%s}", map[cmgr.ChallengeId][]int{"a/one": {1}})
	spring := routedSchema("spring", "event", "flag{%s}", map[cmgr.ChallengeId][]int{"a/two": {2}})
	autumn := routedSchema("autumn", "event", "flag{%s}", map[cmgr.ChallengeId][]int{"a/three": {3}})

	routes, err := routeSchemas([]*cmgr.Schema{year, spring, autumn}, nil, dests)
	if err != nil {
		t.Fatalf("routeSchemas: %s", err)
	}
	if len(routes) != 2 {
		t.Fatalf("%d route(s), want one per orchestrator: %+v", len(routes), routes)
	}
	// Two schemas for one orchestrator arrive as one route, so the payloads
	// they share are assembled together.
	for _, r := range routes {
		switch r.address {
		case "http://library:4200":
			if len(r.schemas) != 1 || r.schemas[0].Name != "year-round" {
				t.Errorf("the library route carries %d schema(s)", len(r.schemas))
			}
		case "http://event:4200":
			if len(r.schemas) != 2 {
				t.Errorf("the event route carries %d schema(s), want both", len(r.schemas))
			}
		default:
			t.Errorf("a route to %q", r.address)
		}
	}

	// --server overrides routing: everything goes where it says, whatever
	// the schemas name.
	routes, err = routeSchemas([]*cmgr.Schema{year, spring}, []string{"http://only:4200"}, dests)
	if err != nil || len(routes) != 1 || len(routes[0].schemas) != 2 {
		t.Fatalf("--server did not override routing: %+v, %v", routes, err)
	}

	// Every unresolvable schema is named at once: an operator fixing one
	// destination line wants to hear about the others too.
	bad := routedSchema("typo-one", "libary", "flag{%s}", map[cmgr.ChallengeId][]int{"a/x": {1}})
	worse := routedSchema("typo-two", "evnet", "flag{%s}", map[cmgr.ChallengeId][]int{"a/y": {1}})
	_, err = routeSchemas([]*cmgr.Schema{bad, worse}, nil, dests)
	if err == nil {
		t.Fatal("schemas naming destinations that do not exist were routed")
	}
	for _, want := range []string{"typo-one", "typo-two"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %s", want, err)
		}
	}
}

// One content-addressed tag cannot be served by two orchestrators: whichever
// drops its schema first retires it from under the other. The build plane is
// the only thing that sees both, so it is the only thing that can refuse it.
func TestExclusivityConflicts(t *testing.T) {
	dests, err := loadDestinations(writeDestinations(t, "library: http://library:4200\nevent: http://event:4200\n"))
	if err != nil {
		t.Fatal(err)
	}
	shared := map[cmgr.ChallengeId][]int{"a/shared": {1}}

	// The same content in two schemas bound to different orchestrators.
	year := routedSchema("year-round", "library", "flag{%s}", shared)
	spring := routedSchema("spring", "event", "flag{%s}", shared)
	routes, err := routeSchemas([]*cmgr.Schema{year, spring}, nil, dests)
	if err != nil {
		t.Fatal(err)
	}
	conflicts := exclusivityConflicts(routes)
	if len(conflicts) != 1 {
		t.Fatalf("%d conflict(s), want the one shared build: %v", len(conflicts), conflicts)
	}
	for _, want := range []string{"a/shared", "seed 1", "year-round", "spring", "library", "event"} {
		if !strings.Contains(conflicts[0], want) {
			t.Errorf("the conflict does not name %q: %s", want, conflicts[0])
		}
	}

	// The same content in two schemas on the SAME orchestrator is fine:
	// contentReferenced keeps a tag while any row still names it.
	together := routedSchema("also-event", "event", "flag{%s}", shared)
	routes, err = routeSchemas([]*cmgr.Schema{spring, together}, nil, dests)
	if err != nil {
		t.Fatal(err)
	}
	if got := exclusivityConflicts(routes); len(got) != 0 {
		t.Errorf("two schemas on one orchestrator sharing content were refused: %v", got)
	}

	// A different flag format is different content, so a different tag, so
	// no conflict -- which is what lets an event and the year-round corpus
	// hold the same challenge when their formats differ.
	otherFormat := routedSchema("spring", "event", "ctf{%s}", shared)
	routes, err = routeSchemas([]*cmgr.Schema{year, otherFormat}, nil, dests)
	if err != nil {
		t.Fatal(err)
	}
	if got := exclusivityConflicts(routes); len(got) != 0 {
		t.Errorf("different flag formats were treated as the same content: %v", got)
	}

	// A different seed likewise.
	otherSeed := routedSchema("spring", "event", "flag{%s}", map[cmgr.ChallengeId][]int{"a/shared": {2}})
	routes, err = routeSchemas([]*cmgr.Schema{year, otherSeed}, nil, dests)
	if err != nil {
		t.Fatal(err)
	}
	if got := exclusivityConflicts(routes); len(got) != 0 {
		t.Errorf("different seeds were treated as the same content: %v", got)
	}
}
