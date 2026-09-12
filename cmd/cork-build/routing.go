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
// serverRouteName is the route name --server produces, which is not a
// destination and is never asked about.
const serverRouteName = "--server"

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
			routes = append(routes, route{name: serverRouteName, address: address, schemas: schemas})
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

// misroutedSchemas refuses a schema this run would hand to one orchestrator
// while another is already serving it. Editing `destination:` and running the
// command every other schema edit takes -- update-schema, or build -- would
// otherwise deploy it to the new orchestrator and converge it there while the
// old one keeps its rows, its tags and its running instances. Both would then
// be serving one content-addressed tag, which is the state exclusivity exists
// to prevent and the state remove-schema cannot resolve without retiring
// images the other is still launching from.
//
// migrate-schema is the operation that does this safely: it releases the
// schema from the old destination without retiring, then converges on the new
// one.
//
// Only asked where the answer can differ: with one destination configured
// there is nowhere else a schema could be served from, so a single-destination
// plane pays nothing and does not become unable to build when some unrelated
// orchestrator is down. --server is not checked at all, being the deliberate
// override of routing, and is what an operator has left when a destination
// cannot be reached.
func misroutedSchemas(routes []route, dests *destinations) []string {
	if dests == nil || len(dests.byName) < 2 {
		return nil
	}
	problems := []string{}
	// One answer per destination per run. A destination that could not be
	// reached is reported once rather than once per schema, and one that was
	// reached is not asked again.
	asked := map[string]error{}
	for _, r := range routes {
		if r.name == serverRouteName {
			continue
		}
		for _, schema := range r.schemas {
			// The destination this schema is routed to, first and on its
			// own. A schema already served by the orchestrator it is being
			// handed to is the ordinary re-deploy, and it is the whole of
			// this run's business: no other destination can make it wrong,
			// so none is asked. That is what keeps a year-round library
			// deployable while last season's event host is switched off --
			// the state this guard refuses needs TWO orchestrators serving
			// one schema, and this one demonstrably serves it.
			if address, ok := dests.byName[r.name]; ok {
				has, err := servesSchema(address, schema.Name)
				if err != nil {
					problems = append(problems, fmt.Sprintf(
						"could not ask %s (%s), the destination schema '%s' names, whether it already serves it: %s", r.name, address, schema.Name, err))
					continue
				}
				if has {
					continue
				}
			}
			// It is not served where it is going, so somewhere else may be
			// serving it, and every other destination has to answer before
			// this run can be sure it is not about to make two.
			where, err := schemaServedElsewhere(schema.Name, r.name, dests, asked)
			if err != nil {
				problems = append(problems, fmt.Sprintf(
					"could not check whether schema '%s' is already served elsewhere, and handing it over without knowing could have two orchestrators serve one tag (--server deploys without this check): %s", schema.Name, err))
				continue
			}
			if where != "" {
				problems = append(problems, fmt.Sprintf(
					"schema '%s' is served by '%s' and this file names '%s': use migrate-schema, which releases it from '%s' without retiring the images both would resolve to", schema.Name, where, r.name, where))
			}
		}
	}
	return problems
}

// schemaServedElsewhere names the destination other than `routedTo` serving
// this schema, if any. `asked` carries each destination's outcome across the
// whole run so that an unreachable one costs one timeout rather than one per
// schema.
func schemaServedElsewhere(schema, routedTo string, dests *destinations, asked map[string]error) (string, error) {
	for name, address := range dests.byName {
		if name == routedTo {
			continue
		}
		if err, seen := asked[name]; seen && err != nil {
			return "", err
		}
		has, err := servesSchema(address, schema)
		if err != nil {
			wrapped := fmt.Errorf("asking %s (%s): %w", name, address, err)
			asked[name] = wrapped
			return "", wrapped
		}
		asked[name] = nil
		if has {
			return name, nil
		}
	}
	return "", nil
}
