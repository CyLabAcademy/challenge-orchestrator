package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/CyLabAcademy/challenge-orchestrator/cmgr"
	"go.yaml.in/yaml/v3"
)

// buildCommand is the whole of what this binary is for: read the schema
// files, record the challenge directory, build and push what the schemas
// name, and hand each challenge over.
func buildCommand(mgr *cmgr.Manager, servers []string, args []string) int {
	if len(args) == 0 {
		return usageError("build takes the schema files to build, and none were given")
	}
	schemas := make([]*cmgr.Schema, 0, len(args))
	refused := false
	for _, path := range args {
		schema, err := loadSchema(path)
		if err != nil {
			// Every file that cannot be built from, not the first: one run
			// should say everything there is to fix.
			fmt.Fprintf(os.Stderr, "error: %s\n", err)
			refused = true
			continue
		}
		schemas = append(schemas, schema)
	}
	if refused {
		return RUNTIME_ERROR
	}

	// The tree first, as an operator runs 'update' before 'update-schema':
	// a converge refuses to build a challenge whose source has moved since
	// it was last recorded, and this is what records it. Its errors are
	// warnings here -- a challenge that does not parse is one no schema can
	// name without the converge below failing on it too, and a corpus is
	// larger than an event.
	fmt.Println("scanning the challenge directory")
	updates := mgr.Update("")
	for _, err := range updates.Errors {
		fmt.Fprintf(os.Stderr, "warning: %s\n", err)
	}
	fmt.Printf("  %d added, %d updated, %d refreshed, %d stale, %d unmodified, %d removed\n",
		len(updates.Added), len(updates.Updated), len(updates.Refreshed),
		len(updates.Stale), len(updates.Unmodified), len(updates.Removed))

	for _, schema := range schemas {
		fmt.Printf("building schema '%s': %d challenge(s), flag format '%s'\n",
			schema.Name, len(schema.Challenges), schema.FlagFormat)
		if errs := mgr.UpdateSchema(forBuilding(schema)); len(errs) > 0 {
			for _, err := range errs {
				fmt.Fprintf(os.Stderr, "error: %s\n", err)
			}
			return RUNTIME_ERROR
		}
	}

	// What each schema built, read back from this plane's own database.
	built := map[string][]*cmgr.ChallengeMetadata{}
	for _, schema := range schemas {
		state, err := mgr.GetSchemaState(schema.Name)
		if err != nil {
			return runtimeError(fmt.Errorf("reading back what schema '%s' built: %w", schema.Name, err))
		}
		built[schema.Name] = state
	}
	// The pins are an input to every build's content identity and the one
	// input the orchestrator cannot derive for itself: they live here.
	fingerprint := mgr.BasePinFingerprint()
	payloads, order := assemble(schemas, built, fingerprint)

	// Every build must be what a hand-over would say it is. The
	// orchestrator asks exactly this of what reaches it and refuses what
	// fails, so asking here turns a refusal over a checksum or a
	// generation into the answer (see identityMismatches).
	identity := func(build *cmgr.BuildMetadata, challengeType string) uint32 {
		return mgr.ContentChecksum(build.SourceChecksum, build.Format, fingerprint, challengeType)
	}
	if mismatched := identityMismatches(payloads, order, identity); len(mismatched) > 0 {
		for _, complaint := range mismatched {
			fmt.Fprintf(os.Stderr, "error: %s\n", complaint)
		}
		fmt.Fprintf(os.Stderr, "error: the orchestrator recomputes what a hand-over states and refuses what disagrees, so none of this would be recorded. Build these again -- from a database that does not hold them (%s) where it is the pins that moved -- and hand the result over.\n", cmgr.DB_ENV)
		return RUNTIME_ERROR
	}

	fmt.Printf("built %d challenge(s)\n", len(order))
	for _, id := range order {
		payload := payloads[id]
		fmt.Printf("  %s: %d build(s), source generation %x\n",
			id, len(payload.Challenge.Builds), payload.Challenge.SourceChecksum)
	}

	if len(servers) == 0 {
		fmt.Println("no --server given: built and pushed, nothing handed over")
		return NO_ERROR
	}
	failed := false
	for _, server := range servers {
		if err := checkServer(server); err != nil {
			fmt.Fprintf(os.Stderr, "error: %s\n", err)
			failed = true
			continue
		}
		fmt.Printf("handing over to %s\n", server)
		for _, id := range order {
			if err := handOver(server, id, payloads[id], mgr.BuildArtifactsPath); err != nil {
				fmt.Fprintf(os.Stderr, "error: %s\n", err)
				failed = true
			}
		}
	}
	if failed {
		return RUNTIME_ERROR
	}
	return NO_ERROR
}

// forBuilding is the schema as this plane converges it: every build on
// demand, whatever the schema asks for. A converge launches nothing for an
// on-demand build, and nothing should run here -- this host has a docker
// daemon to build on, not the workers an event is served from. What the
// schema really asks for reaches the orchestrator in the hand-over, which
// is where the instances belong (see assemble).
func forBuilding(schema *cmgr.Schema) *cmgr.Schema {
	building := &cmgr.Schema{
		Name:       schema.Name,
		FlagFormat: schema.FlagFormat,
		Challenges: make(map[cmgr.ChallengeId]cmgr.BuildSpecification, len(schema.Challenges)),
	}
	for id, spec := range schema.Challenges {
		building.Challenges[id] = cmgr.BuildSpecification{
			Seeds:         spec.Seeds,
			InstanceCount: cmgr.DYNAMIC_INSTANCES,
		}
	}
	return building
}

// assemble turns what this plane just built into one hand-over per
// challenge: the challenge as the scan recorded it, and every build of it
// the schemas name, each carrying the instance count of the schema that
// named it rather than the on-demand one it was converged with. A challenge
// two schemas name arrives once, with the builds of both. `built` is what
// each schema left in this plane's database, by schema name.
func assemble(schemas []*cmgr.Schema, built map[string][]*cmgr.ChallengeMetadata, fingerprint uint32) (map[cmgr.ChallengeId]*cmgr.HandOver, []cmgr.ChallengeId) {
	payloads := map[cmgr.ChallengeId]*cmgr.HandOver{}
	order := []cmgr.ChallengeId{}

	for _, schema := range schemas {
		for _, challenge := range built[schema.Name] {
			builds := challenge.Builds
			payload, known := payloads[challenge.Id]
			if !known {
				challenge.Builds = nil
				payload = &cmgr.HandOver{Challenge: challenge, PinFingerprint: fingerprint}
				payloads[challenge.Id] = payload
				order = append(order, challenge.Id)
			}
			spec := schema.Challenges[challenge.Id]
			for _, build := range builds {
				build.InstanceCount = spec.InstanceCount
				// This plane runs nothing, and what runs on the
				// orchestrator is the orchestrator's own.
				build.Instances = nil
				payload.Challenge.Builds = append(payload.Challenge.Builds, build)
			}
		}
	}
	return payloads, order
}

// pinsCommand re-resolves the base images. It is here rather than on the
// orchestrator because the pins belong with the builds they go into: an
// orchestrator on an external build plane answers 409 to the pin endpoints,
// having no tree to read the bases from and nothing to build with them.
func pinsCommand(mgr *cmgr.Manager) int {
	pins, err := mgr.RefreshBasePins()
	for _, pin := range pins {
		fmt.Printf("%-30s %-71s used by %d\n", pin.Ref, pin.Digest, pin.InUse)
	}
	// Only when the refresh got as far as looking: RefreshBasePins answers
	// no pins and an error when the plane is external, when pinning is not
	// configured, or when the scan itself failed, and saying the tree names
	// no base then is a statement about the tree that was never made.
	if len(pins) == 0 && err == nil {
		fmt.Println("the challenge directory names no base image to pin")
	}
	if err != nil {
		// A refresh can partially succeed: some pins written, others left
		// on a mutable tag.
		return runtimeError(err)
	}
	fmt.Println()
	fmt.Println("Pins refreshed. Nothing is rebuilt on account of this: a moved base")
	fmt.Println("reaches a challenge the next time that challenge is built, so the")
	fmt.Println("working order is to refresh and then build.")
	return NO_ERROR
}

// loadSchema reads a schema file, as cmgrd-cli reads one for the
// orchestrator: the same yaml or json, and here it is what decides what is
// built.
func loadSchema(path string) (*cmgr.Schema, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	schema := new(cmgr.Schema)
	switch filepath.Ext(path) {
	case ".json":
		err = json.Unmarshal(data, schema)
	case ".yaml", ".yml":
		err = yaml.Unmarshal(data, schema)
	default:
		err = fmt.Errorf("unrecognized schema file extension on '%s' (expected .yaml or .json)", path)
	}
	if err != nil {
		return nil, err
	}
	if schema.Name == "" {
		return nil, fmt.Errorf("schema file '%s' has no name", path)
	}
	if schema.FlagFormat == "" {
		return nil, fmt.Errorf("schema file '%s' has no flag format", path)
	}
	if len(schema.Challenges) == 0 {
		return nil, fmt.Errorf("schema file '%s' names no challenges", path)
	}
	// A challenge with no seeds asks for no build, so nothing would be
	// built for it and nothing handed over, and the run would end well:
	// 'seed' for 'seeds' is how that gets written by accident. All of them
	// at once, so one run says everything there is to fix.
	seedless := []string{}
	for id, spec := range schema.Challenges {
		if len(spec.Seeds) == 0 {
			seedless = append(seedless, "'"+string(id)+"'")
		}
	}
	if len(seedless) > 0 {
		slices.Sort(seedless)
		return nil, fmt.Errorf("schema file '%s' names no seeds for %s, so nothing would be built for them",
			path, strings.Join(seedless, ", "))
	}
	return schema, nil
}

// identityMismatches names every build that is not what a hand-over would
// say it is, in the two ways a build plane can get that wrong. Both are
// what the orchestrator recomputes and refuses on (checkHandOver), so both
// are worth catching here, where the answer is legible, rather than there.
//
// A build is stamped with its source generation and its content identity
// when it is made. A rebuild that failed leaves it at the generation it
// still serves while the challenge has moved on; and a base image pin
// refresh rebuilds nothing by design (see BUILDER.md), so a build carried
// over in a kept database from before a refresh still carries the identity
// it was made under.
func identityMismatches(payloads map[cmgr.ChallengeId]*cmgr.HandOver, order []cmgr.ChallengeId, identity func(build *cmgr.BuildMetadata, challengeType string) uint32) []string {
	complaints := []string{}
	for _, id := range order {
		challenge := payloads[id].Challenge
		for _, build := range challenge.Builds {
			where := fmt.Sprintf("the build of '%s' for schema '%s' (format '%s', seed %d)",
				id, build.Schema, build.Format, build.Seed)
			if build.SourceChecksum != challenge.SourceChecksum {
				complaints = append(complaints, fmt.Sprintf(
					"%s was made from source generation %x and the challenge is at %x: its rebuild failed, and the scan above says why",
					where, build.SourceChecksum, challenge.SourceChecksum))
				continue
			}
			if want := identity(build, challenge.ChallengeType); build.Checksum != want {
				complaints = append(complaints, fmt.Sprintf(
					"%s carries content identity %x, and the inputs it would be handed over with give %x: it was made under other base image pins",
					where, build.Checksum, want))
			}
		}
	}
	return complaints
}
