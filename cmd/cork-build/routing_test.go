package main

import (
	"net"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/CyLabAcademy/challenge-orchestrator/cork"
)

func routedSchema(name, destination, format string, challenges map[cork.ChallengeId][]int) *cork.Schema {
	s := &cork.Schema{
		Name:        name,
		Destination: destination,
		FlagFormat:  format,
		Challenges:  map[cork.ChallengeId]cork.BuildSpecification{},
	}
	for id, seeds := range challenges {
		s.Challenges[id] = cork.BuildSpecification{Seeds: seeds, InstanceCount: cork.DYNAMIC_INSTANCES}
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
	year := routedSchema("year-round", "library", "flag{%s}", map[cork.ChallengeId][]int{"a/one": {1}})
	spring := routedSchema("spring", "event", "flag{%s}", map[cork.ChallengeId][]int{"a/two": {2}})
	autumn := routedSchema("autumn", "event", "flag{%s}", map[cork.ChallengeId][]int{"a/three": {3}})

	routes, err := routeSchemas([]*cork.Schema{year, spring, autumn}, nil, dests)
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
	routes, err = routeSchemas([]*cork.Schema{year, spring}, []string{"http://only:4200"}, dests)
	if err != nil || len(routes) != 1 || len(routes[0].schemas) != 2 {
		t.Fatalf("--server did not override routing: %+v, %v", routes, err)
	}

	// Every unresolvable schema is named at once: an operator fixing one
	// destination line wants to hear about the others too.
	bad := routedSchema("typo-one", "libary", "flag{%s}", map[cork.ChallengeId][]int{"a/x": {1}})
	worse := routedSchema("typo-two", "evnet", "flag{%s}", map[cork.ChallengeId][]int{"a/y": {1}})
	_, err = routeSchemas([]*cork.Schema{bad, worse}, nil, dests)
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
	shared := map[cork.ChallengeId][]int{"a/shared": {1}}

	// The same content in two schemas bound to different orchestrators.
	year := routedSchema("year-round", "library", "flag{%s}", shared)
	spring := routedSchema("spring", "event", "flag{%s}", shared)
	routes, err := routeSchemas([]*cork.Schema{year, spring}, nil, dests)
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
	routes, err = routeSchemas([]*cork.Schema{spring, together}, nil, dests)
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
	routes, err = routeSchemas([]*cork.Schema{year, otherFormat}, nil, dests)
	if err != nil {
		t.Fatal(err)
	}
	if got := exclusivityConflicts(routes); len(got) != 0 {
		t.Errorf("different flag formats were treated as the same content: %v", got)
	}

	// A different seed likewise.
	otherSeed := routedSchema("spring", "event", "flag{%s}", map[cork.ChallengeId][]int{"a/shared": {2}})
	routes, err = routeSchemas([]*cork.Schema{year, otherSeed}, nil, dests)
	if err != nil {
		t.Fatal(err)
	}
	if got := exclusivityConflicts(routes); len(got) != 0 {
		t.Errorf("different seeds were treated as the same content: %v", got)
	}
}

// A schema already served by one orchestrator and routed to another is
// refused, and the refusal names migrate-schema. This is the state one
// content-addressed tag served by two orchestrators, which is what an
// operator produces by editing `destination:` and running the command every
// other schema edit takes.
func TestMisroutedSchemaIsRefused(t *testing.T) {
	library, _ := orchestratorServing(t, "spring")
	event, _ := orchestratorServing(t)
	dests := &destinations{byName: map[string]string{"library": library, "event": event}}

	routes := []route{{name: "event", address: event, schemas: []*cork.Schema{{Name: "spring"}}}}
	problems := misroutedSchemas(routes, dests)
	if len(problems) != 1 {
		t.Fatalf("a schema served by library and routed to event gave %d problem(s): %v", len(problems), problems)
	}
	for _, want := range []string{"spring", "library", "event", "migrate-schema"} {
		if !strings.Contains(problems[0], want) {
			t.Errorf("the refusal does not name %q: %s", want, problems[0])
		}
	}
}

// The schema going back to the destination already serving it is the
// ordinary re-deploy, and a schema nobody serves is the first one. Neither
// is refused: a guard that stopped these would break every routine build.
func TestRoutedWhereItAlreadyLivesIsFine(t *testing.T) {
	library, _ := orchestratorServing(t, "spring")
	event, _ := orchestratorServing(t)
	dests := &destinations{byName: map[string]string{"library": library, "event": event}}

	for _, tc := range []struct {
		name   string
		routes []route
	}{
		{"re-deploy to the same destination", []route{{name: "library", address: library, schemas: []*cork.Schema{{Name: "spring"}}}}},
		{"a schema nobody serves yet", []route{{name: "event", address: event, schemas: []*cork.Schema{{Name: "autumn"}}}}},
	} {
		if problems := misroutedSchemas(tc.routes, dests); len(problems) != 0 {
			t.Errorf("%s was refused: %v", tc.name, problems)
		}
	}
}

// With one destination configured there is nowhere else a schema could be
// served from, so nothing is asked at all. That keeps a single-destination
// plane -- the classroom deployment -- from being unable to build because
// some orchestrator is unreachable, and costs it no requests.
func TestOneDestinationAsksNothing(t *testing.T) {
	only, seen := orchestratorServing(t, "spring")
	dests := &destinations{byName: map[string]string{"library": only}, defName: "library"}
	routes := []route{{name: "event", address: only, schemas: []*cork.Schema{{Name: "spring"}}}}
	if problems := misroutedSchemas(routes, dests); len(problems) != 0 {
		t.Errorf("a single-destination plane was refused: %v", problems)
	}
	if len(*seen) != 0 {
		t.Errorf("a single-destination plane made %d request(s), want none", len(*seen))
	}
}

// --server is the deliberate override of routing and is never checked: it is
// also what an operator has left when a destination cannot be reached.
func TestServerRouteIsNotChecked(t *testing.T) {
	library, seen := orchestratorServing(t, "spring")
	other, _ := orchestratorServing(t)
	dests := &destinations{byName: map[string]string{"library": library, "event": other}}
	routes := []route{{name: serverRouteName, address: other, schemas: []*cork.Schema{{Name: "spring"}}}}
	if problems := misroutedSchemas(routes, dests); len(problems) != 0 {
		t.Errorf("--server was refused: %v", problems)
	}
	if len(*seen) != 0 {
		t.Errorf("--server asked %d question(s), want none", len(*seen))
	}
}

// An unrelated destination being unreachable does not stop a deploy to a
// healthy one. This is the year-round case: a library orchestrator keeps
// being deployed to while last season's event host is switched off, and the
// guard must not make every configured destination a hard dependency of
// every build. A schema already served where it is going cannot be made
// wrong by any other orchestrator, so no other is asked.
func TestAnUnreachableNeighbourDoesNotBlockARedeploy(t *testing.T) {
	library, seen := orchestratorServing(t, "library-2026")
	dests := &destinations{byName: map[string]string{
		"library": library,
		"event":   closedAddress(t),
	}}
	routes := []route{{name: "library", address: library, schemas: []*cork.Schema{{Name: "library-2026"}}}}

	if problems := misroutedSchemas(routes, dests); len(problems) != 0 {
		t.Fatalf("an ordinary re-deploy was refused because an unrelated destination was down: %v", problems)
	}
	if len(*seen) != 1 {
		t.Errorf("the routed destination was asked %d times, want exactly 1", len(*seen))
	}
}

// But a schema its own destination does NOT serve still has to be checked
// against the others, and an unreachable one there is a refusal rather than
// a guess -- the state this guard exists to prevent cannot be ruled out.
func TestAnUnreachableNeighbourStillRefusesAnUnknownSchema(t *testing.T) {
	event, _ := orchestratorServing(t)
	dests := &destinations{byName: map[string]string{
		"event":   event,
		"library": closedAddress(t),
	}}
	routes := []route{{name: "event", address: event, schemas: []*cork.Schema{{Name: "spring"}}}}

	problems := misroutedSchemas(routes, dests)
	if len(problems) != 1 {
		t.Fatalf("got %d problem(s), want 1: %v", len(problems), problems)
	}
	if !strings.Contains(problems[0], "--server") {
		t.Errorf("the refusal does not name the operator's way through: %s", problems[0])
	}
}

// A destination that cannot be reached is asked once for the whole run, not
// once per schema. Five schemas against a blackholing host would otherwise
// cost five timeouts before any work started.
func TestAnUnreachableNeighbourIsAskedOncePerRun(t *testing.T) {
	event, _ := orchestratorServing(t)
	// A neighbour that counts how often it is dialled and refuses every
	// time. Counting is the whole point: an assertion on the problems alone
	// passes just as happily with no caching at all, which is what three
	// reviewers said about the version of this test that did that.
	var dialled atomic.Int32
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			dialled.Add(1)
			conn.Close()
		}
	}()

	dests := &destinations{byName: map[string]string{
		"event":   event,
		"library": "http://" + listener.Addr().String(),
	}}
	routes := []route{{name: "event", address: event, schemas: []*cork.Schema{
		{Name: "one"}, {Name: "two"}, {Name: "three"},
	}}}

	problems := misroutedSchemas(routes, dests)
	if len(problems) != 3 {
		t.Errorf("got %d problem(s), want one per schema: %v", len(problems), problems)
	}
	for _, p := range problems {
		if !strings.Contains(p, "already served elsewhere") {
			t.Errorf("unexpected problem text: %s", p)
		}
	}
	// Every schema is reported, but the dead host was dialled once: the rest
	// reuse the recorded failure. Without the cache this is 3.
	if got := dialled.Load(); got != 1 {
		t.Errorf("the unreachable destination was dialled %d times for 3 schemas, want 1: a dead host costs one timeout per run, not per schema", got)
	}
}
