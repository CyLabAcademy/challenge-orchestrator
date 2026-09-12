package main

import (
	"fmt"
	"sort"

	"github.com/CyLabAcademy/challenge-orchestrator/cmgr"
)

// A route is one orchestrator and the schemas it serves. Schemas are grouped
// before anything is handed over, because assemble merges a challenge two
// schemas name into one payload with the builds of both -- right when both
// schemas are served by the same orchestrator, and wrong when they are not.
// Grouping first means each orchestrator is told about the schemas it serves
// and no others.
type route struct {
	name    string // the destination as a schema names it, or "--server"
	address string
	schemas []*cmgr.Schema
}

// routeSchemas works out where each schema goes.
//
// --server overrides routing entirely and sends everything to each address
// given: one orchestrator, or a test. Otherwise every schema is resolved
// against the configured destinations, and a schema that cannot be resolved
// stops the run before anything is handed anywhere -- all of them named at
// once, since an operator fixing one destination line wants to know about
// the others too.
func routeSchemas(schemas []*cmgr.Schema, servers []string, dests *destinations) ([]route, error) {
	if len(servers) > 0 {
		routes := make([]route, 0, len(servers))
		for _, address := range servers {
			routes = append(routes, route{name: "--server", address: address, schemas: schemas})
		}
		return routes, nil
	}

	byAddress := map[string]*route{}
	order := []string{}
	problems := []string{}
	for _, schema := range schemas {
		address, err := dests.resolve(schema.Name, schema.Destination)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		r, seen := byAddress[address]
		if !seen {
			name := schema.Destination
			if name == "" {
				name = dests.defName
			}
			r = &route{name: name, address: address}
			byAddress[address] = r
			order = append(order, address)
		}
		r.schemas = append(r.schemas, schema)
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf("%s", joinLines(problems))
	}

	routes := make([]route, 0, len(order))
	for _, address := range order {
		routes = append(routes, *byAddress[address])
	}
	return routes, nil
}

// exclusivityConflicts names every build two orchestrators would both serve.
//
// A build's tag is content-addressed, so two schemas naming the same
// challenge at the same seed and flag format resolve to the same tag no
// matter which orchestrator they are bound to. One tag, two owners: when
// either removes its schema it retires that tag, and the other is left
// serving images the registry no longer has. The build plane is the only
// thing that can see both, so it is the only thing that can refuse this.
//
// Within one orchestrator the same content is fine -- contentReferenced
// keeps a tag while any row still names it -- so only pairs across routes
// are conflicts.
//
// It sees what this run was given and no more: a schema's destination is
// recorded nowhere between runs (the database keys builds by schema name and
// holds no destination), so schemas that share a challenge have to be built
// together for the rule to bind. Said so in the usage text and in
// BUILDER.md, because it is a habit rather than something enforceable here.
func exclusivityConflicts(routes []route) []string {
	type owner struct{ destination, schema string }
	owners := map[string]owner{}
	conflicts := []string{}

	for _, r := range routes {
		for _, schema := range r.schemas {
			for id, spec := range schema.Challenges {
				for _, seed := range spec.Seeds {
					// What the tag is derived from, as far as a schema
					// decides it: the rest (source, pins, template) is the
					// same tree for every schema in this run.
					key := fmt.Sprintf("%s\x00%d\x00%s", id, seed, schema.FlagFormat)
					held, taken := owners[key]
					if !taken {
						owners[key] = owner{r.name, schema.Name}
						continue
					}
					if held.destination == r.name {
						continue // same orchestrator: shared content is fine
					}
					conflicts = append(conflicts, fmt.Sprintf(
						"'%s' at seed %d with flag format '%s' is named by schema '%s' for destination '%s' and schema '%s' for destination '%s': one content-addressed tag cannot be served by two orchestrators, because whichever drops its schema first retires it from under the other",
						id, seed, schema.FlagFormat, held.schema, held.destination, schema.Name, r.name))
				}
			}
		}
	}
	sort.Strings(conflicts)
	return conflicts
}

func joinLines(lines []string) string {
	out := ""
	for i, line := range lines {
		if i > 0 {
			out += "\n"
		}
		out += line
	}
	return out
}
