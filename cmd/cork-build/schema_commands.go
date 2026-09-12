package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/CyLabAcademy/challenge-orchestrator/cmgr"
)

// removeSchemaCommand takes a schema out of service for good: the
// orchestrator serving it drops its builds and retires their tags, and this
// build plane drops its own rows for them. Which orchestrator that is, is
// asked rather than read off the file (see removalTarget).
//
// Both halves matter. The orchestrator's rows are what serves the event; the
// build plane's are what would otherwise make the next `build` of the same
// schema a no-op, because a build row that still carries a flag is a build
// generateBuilds considers done -- it would push nothing, and the hand-over
// would then be refused for a tag the orchestrator had just retired. A
// removal that only reached one side would brick the schema on the next add.
func removeSchemaCommand(mgr *cmgr.Manager, servers []string, dests *destinations, args []string) int {
	if len(args) != 1 {
		return usageError("remove-schema takes one schema name (list-schemas names them)")
	}
	if len(servers) > 1 {
		return usageError("remove-schema acts on one orchestrator, and %d --server were given", len(servers))
	}
	// A name, as cmgr's remove-schema took and as cmgrd-cli's takes. There
	// is nothing in the file for a removal to read: the destination comes
	// from asking which orchestrator is serving the schema, and everything
	// else about it is being destroyed. Taking a file would only mean the
	// operator had to still have one -- and it is what made removing every
	// schema a loop over files rather than over `list-schemas`.
	schemaName := args[0]
	// Before the name is checked at all, because a path fails that check for
	// the wrong reason: '/tmp/spring.yaml' contains a slash, and being told
	// it cannot address a schema over HTTP is true and no help. This command
	// took a file until recently, so a caller still passing one is the
	// likeliest way to get here.
	if ext := filepath.Ext(schemaName); ext == ".yaml" || ext == ".yml" || ext == ".json" {
		return usageError("remove-schema takes a schema name, and '%s' is a schema file -- it used to take the file. Pass the name inside it (list-schemas names them)", schemaName)
	}
	if err := validSchemaName(schemaName); err != nil {
		return usageError("schema name: %s", err)
	}
	name, address, err := removalTarget(schemaName, servers, dests)
	if err != nil {
		return runtimeError(err)
	}

	if address == "" {
		// Nothing is serving it, so whether there is anything to remove at
		// all is this plane's own record's question -- and if the answer is
		// also no, then nothing by that name exists anywhere and saying
		// "removed" would be a lie. Every legitimate removal has one or the
		// other: a schema in service, or rows a half-finished migration
		// left behind.
		known, listErr := mgr.ListSchemas()
		if listErr != nil {
			return runtimeError(fmt.Errorf("reading the schemas this build plane has built: %w", listErr))
		}
		if !slices.Contains(known, schemaName) {
			return runtimeError(fmt.Errorf("there is no schema '%s' to remove: no configured destination is serving it, and this build plane has no rows for it either (list-schemas names what there is)", schemaName))
		}
		// Served nowhere, so the rows here are all that is left of it and
		// dropping them is the whole of what remains. This is the state a
		// migration that failed after its release leaves, and saying it is
		// how an operator tells that apart from a removal that did nothing.
		//
		// Its images stay in the registry, and are said to: retiring a tag
		// is an orchestrator's doing (retireRegistryTag refuses it on a
		// build plane, which pushes and never takes back), so with none
		// holding this schema there is nobody left to retire them.
		fmt.Printf("no configured destination is serving schema '%s'; removing this build plane's rows for it, and nothing else -- the images it pushed stay in the registry, since retiring a tag is the serving orchestrator's doing and none has this schema\n", schemaName)
	} else {
		fmt.Printf("removing schema '%s' from %s (%s)\n", schemaName, name, address)
		if err := deleteSchemaOn(address, schemaName, true); err != nil {
			return runtimeError(err)
		}
	}
	// Only once the orchestrator has answered: if it refused, this plane's
	// rows are the record of what is still out there.
	if err := mgr.DeleteSchema(schemaName, true); err != nil {
		return runtimeError(fmt.Errorf("the orchestrator removed schema '%s', and this build plane could not drop its own rows for it -- the next build of it would push nothing: %w", schemaName, err))
	}
	if address == "" {
		fmt.Printf("schema '%s' removed: its rows are gone from here\n", schemaName)
	} else {
		fmt.Printf("schema '%s' removed: its builds are gone from %s, its tags from the registry, and its rows from here\n", schemaName, name)
	}
	return NO_ERROR
}

// migrateSchemaCommand moves a schema from the orchestrator serving it to
// the one its file now names.
//
// Nothing is rebuilt. A build's identity is its source, flag format, base
// pins and template -- none of which a destination touches -- so the tag the
// new orchestrator resolves is the tag the old one was serving. The release
// therefore must not retire it (?retire=false), and the hand-over that
// follows adopts what is already in the registry.
//
// The order is what makes it safe: released first, then handed over. Both
// orchestrators serving one content-addressed tag is the state the
// exclusivity rule exists to prevent, and a failure after the release leaves
// the schema served nowhere, which re-running fixes.
func migrateSchemaCommand(mgr *cmgr.Manager, servers []string, dests *destinations, args []string) int {
	if len(servers) > 0 {
		return usageError("migrate-schema moves a schema between destinations, so it cannot be given --server")
	}
	if len(args) != 1 {
		return usageError("migrate-schema takes the schema file whose destination has changed")
	}
	schema, err := loadSchema(args[0])
	if err != nil {
		return runtimeError(err)
	}
	// Said here rather than left to resolve, whose advice for a build plane
	// with nothing configured is "set it, or give --server" -- written for
	// `build`, and not on offer to a migration, which moves a schema between
	// configured destinations and so refuses --server above.
	if len(dests.byName) == 0 {
		return runtimeError(fmt.Errorf("no destinations are configured (%s), and migrate-schema moves a schema between them: configure them, then `build` converges schema '%s' onto the one it names", DESTINATIONS_ENV, schema.Name))
	}
	to, err := dests.resolve(schema.Name, schema.Destination)
	if err != nil {
		return runtimeError(err)
	}
	toName := schema.Destination
	if toName == "" {
		toName = dests.defName
	}

	// Where it is now. The build plane records no assignment of its own --
	// the schema files are the record, and this one has already been edited
	// -- so the orchestrator that still has the schema is the one to ask,
	// and it is found by asking them.
	fromName, from, err := whereSchemaLives(schema.Name, dests)
	if err != nil {
		return runtimeError(err)
	}
	if from == "" {
		return runtimeError(fmt.Errorf("no configured destination is serving schema '%s': there is nothing to migrate, and `build` would add it to '%s'", schema.Name, toName))
	}
	if from == to {
		return runtimeError(fmt.Errorf("schema '%s' is already served by '%s': `build` re-converges it there, and migrate-schema is for moving it elsewhere", schema.Name, toName))
	}

	fmt.Printf("migrating schema '%s' from %s (%s) to %s (%s)\n", schema.Name, fromName, from, toName, to)
	// Released, not destroyed: the tag is content-addressed, so the
	// orchestrator taking this schema resolves the same one.
	if err := deleteSchemaOn(from, schema.Name, false); err != nil {
		return runtimeError(fmt.Errorf("releasing schema '%s' from %s: %w", schema.Name, fromName, err))
	}
	fmt.Printf("released from %s, its images left in the registry for %s\n", fromName, toName)

	// The rows here are dropped too, so the converge below re-derives them
	// against the destination the file now names.
	if err := mgr.DeleteSchema(schema.Name, true); err != nil {
		// `build`, not migrate-schema: the release above already happened,
		// so nothing is serving this schema now and a second migrate-schema
		// would refuse for having nothing to move. Its rows and its images
		// are both still here, which is all the converge needs.
		return runtimeError(fmt.Errorf("schema '%s' was released from %s and this build plane could not drop its own rows for it: run `build` to converge it onto %s: %w", schema.Name, fromName, toName, err))
	}
	return buildCommand(mgr, nil, dests, args)
}

// addSchemaCommand and updateSchemaCommand are `build` for one schema, with
// the guardrail each name has always carried: add refuses a schema this
// plane has built before, update refuses one it has not.
//
// They do the same work because on a build plane there is only one piece of
// work -- build what the schema names, push it, hand it to the destination,
// converge it there -- and `build` is that work for any number of schemas,
// indifferent to whether it has seen them. The difference is the refusal:
// `add-schema` on a name already in use is almost always a second event
// written against the first one's schema, and `update-schema` on a name
// that is not is almost always a typo. `build` is the form that does not
// ask, and is the one to use for a run of several schemas, since exclusivity
// is only checked across the schemas of one run.
func addSchemaCommand(mgr *cmgr.Manager, servers []string, dests *destinations, args []string) int {
	return deploySchema(mgr, servers, dests, args, "add-schema", false)
}

func updateSchemaCommand(mgr *cmgr.Manager, servers []string, dests *destinations, args []string) int {
	return deploySchema(mgr, servers, dests, args, "update-schema", true)
}

func deploySchema(mgr *cmgr.Manager, servers []string, dests *destinations, args []string, command string, wantKnown bool) int {
	if len(args) != 1 {
		return usageError("%s takes one schema file", command)
	}
	schema, err := loadSchema(args[0])
	if err != nil {
		return runtimeError(err)
	}
	// What this plane has built, which is what it means for a schema to
	// exist here: the database keys builds by schema name and holds no
	// schema table of its own (schemaExists, queryForSchemas).
	known, err := mgr.ListSchemas()
	if err != nil {
		return runtimeError(fmt.Errorf("reading the schemas this build plane has built: %w", err))
	}
	if err := schemaGuard(schema.Name, known, wantKnown); err != nil {
		return runtimeError(err)
	}
	return buildCommand(mgr, servers, dests, args)
}

// schemaGuard is the only thing that separates add-schema from
// update-schema: whether the plane is expected to have built this schema
// before. Both then do the same work.
func schemaGuard(schemaName string, known []string, wantKnown bool) error {
	built := slices.Contains(known, schemaName)
	switch {
	case wantKnown && !built:
		// Worth naming the database: it is bookkeeping and may be thrown
		// away, so a plane rebuilt since the schema was deployed has no row
		// for a schema an orchestrator is serving right now. `build` is the
		// way through that, and it re-derives rather than rebuilding.
		return fmt.Errorf("this build plane has built nothing for schema '%s', so there is no definition to converge to: add-schema is the first one, and build does either -- and if this plane's database was rebuilt since, build re-derives what the registry already holds", schemaName)
	case !wantKnown && built:
		return fmt.Errorf("this build plane has already built schema '%s': update-schema converges it to a new definition, and build does either", schemaName)
	}
	return nil
}

// listSchemasCommand names the schemas this build plane has built. Not what
// is being served: an orchestrator holds that, and holds only its own.
func listSchemasCommand(mgr *cmgr.Manager, args []string) int {
	if len(args) != 0 {
		return usageError("list-schemas takes no arguments")
	}
	schemas, err := mgr.ListSchemas()
	if err != nil {
		return runtimeError(err)
	}
	for _, schema := range schemas {
		fmt.Println(schema)
	}
	return NO_ERROR
}

// showSchemaCommand prints what this plane built for a schema, as json on
// stdout so it stays pipeable, and says on stderr which destination is
// serving it -- the question an operator actually has, and the one only a
// build plane can answer, since it is the only thing that sees them all.
//
// Best-effort on that second half: a destination that cannot be reached is
// reported and not fatal. This is a read, and refusing to describe what was
// built because some other orchestrator is down would be the wrong trade.
func showSchemaCommand(mgr *cmgr.Manager, dests *destinations, args []string) int {
	if len(args) != 1 {
		return usageError("show-schema takes one schema name")
	}
	name := args[0]
	if err := validSchemaName(name); err != nil {
		return usageError("schema name: %s", err)
	}
	state, err := mgr.GetSchemaState(name)
	if err != nil {
		return runtimeError(err)
	}
	data, err := json.MarshalIndent(state, "", "    ")
	if err != nil {
		return runtimeError(err)
	}
	fmt.Println(string(data))

	switch where, address, err := whereSchemaLives(name, dests); {
	case err != nil:
		fmt.Fprintf(os.Stderr, "note: could not work out which orchestrator serves '%s': %s\n", name, err)
	case address == "":
		fmt.Fprintf(os.Stderr, "note: no configured destination is serving '%s'\n", name)
	default:
		fmt.Fprintf(os.Stderr, "note: served by %s (%s)\n", where, address)
	}
	return NO_ERROR
}

// destinationsCommand says what routing this build plane has.
func destinationsCommand(dests *destinations) int {
	fmt.Print(dests.listing())
	return NO_ERROR
}

// listing is what `destinations` prints: every name, the orchestrator it
// stands for, and which one an unrouted schema means. Sorted, because it is
// a listing and the same configuration should read the same way twice.
func (d *destinations) listing() string {
	if len(d.byName) == 0 {
		return fmt.Sprintf("no destinations configured (%s)\n", DESTINATIONS_ENV)
	}
	names := make([]string, 0, len(d.byName))
	for name := range d.byName {
		names = append(names, name)
	}
	sort.Strings(names)

	out := &strings.Builder{}
	for _, name := range names {
		note := ""
		if name == d.defName {
			note = "   (the default: a schema naming no destination means this one)"
		}
		fmt.Fprintf(out, "%-20s %s%s\n", name, d.byName[name], note)
	}
	return out.String()
}

// validSchemaName refuses a name that could not address a schema over HTTP.
//
// A name goes into the request path as it is written -- the daemon splits
// the path and takes the last segment (existingSchemaHandler), so escaping
// it would address something else -- which makes a '/' or a '?' in one not a
// naming question but a request aimed somewhere unintended. Checked where a
// name is read from a file and where it is typed as an argument, since
// remove-schema and show-schema take one directly.
// Only what actually breaks, which was measured rather than guessed: a name
// is escaped on the way out and decoded again by the daemon, so a space or a
// non-ASCII letter survives it whole ("Spring CTF 2026" arrives as itself).
// What does not survive is anything that re-divides the request -- a '/' or
// an encoded one leaves the last segment, '?' and '#' leave the first -- and
// a backslash or a control character, which net/url refuses outright.
//
// Its complaints say only what is wrong with the name; each caller frames
// it, since one is reading a file and the others an argument.
func validSchemaName(name string) error {
	if name == "" {
		return fmt.Errorf("has no name")
	}
	if i := strings.IndexAny(name, "/?#%\\\t\r\n"); i >= 0 {
		return fmt.Errorf("'%s' contains %q, which cannot address a schema over HTTP: the name is put into the request path, and that would re-divide it", name, name[i:i+1])
	}
	// And not a schema file's name: remove-schema takes a name and refuses
	// an argument with one of these extensions, on the grounds that it is a
	// caller still passing the file. A schema actually called 'spring.yaml'
	// could then never be removed, so it cannot be called that.
	for _, ext := range []string{".yaml", ".yml", ".json"} {
		if strings.HasSuffix(strings.ToLower(name), ext) {
			return fmt.Errorf("'%s' ends in %s, which reads as a file rather than a schema: remove-schema takes a name and would refuse it as one", name, ext)
		}
	}
	// Not schema names, and they address something else: the daemon serves
	// on net/http's default mux, which cleans a path, so '/schemas/..' would
	// arrive at the collection handler rather than at any schema.
	if name == "." || name == ".." {
		return fmt.Errorf("'%s' is a path, not a schema name", name)
	}
	return nil
}

// removalTarget answers which orchestrator a removal is aimed at: the one
// serving the schema, found by asking, with --server overriding. An empty
// address means no configured destination is serving it, which is a state to
// report rather than an error.
//
// Where it IS, not where the file says it should be. The two differ exactly
// when a destination was edited and not migrated, and removing from the
// address the file now names would answer 204 -- DeleteSchema over a schema
// a daemon has no rows for removes nothing and fails at nothing -- while the
// orchestrator actually serving it kept every build and instance. The
// removal would then have dropped this plane's rows, which is what a later
// build needs to push again, and left the event running with no way back to
// it. migrate-schema asks the same question for the same reason.
func removalTarget(schemaName string, servers []string, dests *destinations) (string, string, error) {
	if len(servers) == 1 {
		return "--server", servers[0], nil
	}
	// "Nowhere is serving it" is a conclusion drawn from asking, and with
	// nothing configured there is nobody to ask. Refused rather than
	// answered, since the answer would read the same as it does for a
	// schema genuinely served nowhere and would drop this plane's rows for
	// one an orchestrator is still running.
	if len(dests.byName) == 0 {
		return "", "", fmt.Errorf("no destinations are configured (%s), so there is nothing to ask where schema '%s' is served: set it, or name the orchestrator with --server", DESTINATIONS_ENV, schemaName)
	}
	name, address, err := whereSchemaLives(schemaName, dests)
	if err != nil {
		return "", "", fmt.Errorf("%w -- or name the orchestrator with --server", err)
	}
	return name, address, nil
}

// whereSchemaLives asks every configured destination whether it is serving a
// schema. Exactly one may: two would be the state exclusivityConflicts
// refuses, and is reported rather than guessed at.
func whereSchemaLives(schema string, dests *destinations) (string, string, error) {
	foundName, foundAddress := "", ""
	for name, address := range dests.byName {
		has, err := servesSchema(address, schema)
		if err != nil {
			return "", "", fmt.Errorf("asking %s (%s) whether it serves schema '%s': %w", name, address, schema, err)
		}
		if !has {
			continue
		}
		if foundAddress != "" {
			// The state, not a remedy: what to do about it differs between
			// the callers, and migrate-schema has no --server to offer.
			return "", "", fmt.Errorf("schema '%s' is served by both '%s' and '%s', and only one of them can be: take it off one before moving or removing it", schema, foundName, name)
		}
		foundName, foundAddress = name, address
	}
	return foundName, foundAddress, nil
}

// servesSchema asks whether an orchestrator holds a schema, which is the
// state it returns for one having challenges in it.
//
// Not the status: GET /schemas/<name> is a query over the builds table, so a
// schema this daemon has never heard of is an empty list and a 200, not a
// 404 (GetSchemaState, getSchemaBuilds). Reading the status alone would make
// every destination look like it served every schema. 404 is still honoured
// for a daemon that grows one.
func servesSchema(address, schema string) (bool, error) {
	resp, err := probeClient().Get(address + "/schemas/" + schema)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("answered %s", resp.Status)
	}
	var state []json.RawMessage
	if err := json.NewDecoder(io.LimitReader(resp.Body, answerLimit)).Decode(&state); err != nil {
		return false, fmt.Errorf("reading what it said it serves: %w", err)
	}
	return len(state) > 0, nil
}

// its tags are spent (a removal) or are about to be served by another
// orchestrator (a migration).
func deleteSchemaOn(address, schema string, retire bool) error {
	url := address + "/schemas/" + schema
	if !retire {
		url += "?retire=false"
	}
	req, err := http.NewRequest(http.MethodDelete, url, nil)
	if err != nil {
		return fmt.Errorf("removing schema '%s' from %s: %w", schema, address, err)
	}
	resp, err := httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("removing schema '%s' from %s: %w", schema, address, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("removing schema '%s' from %s: %s: %s", schema, address, resp.Status, readAnswer(resp))
	}
	return nil
}

// probeClient is what a question uses, and a question is answered out of the
// database or not at all. Bounded where httpClient is not, because
// whereSchemaLives asks every configured destination in turn before anything
// can move: a host that swallows the connection rather than refusing it --
// a firewalled address, a machine that is gone -- would otherwise hang a
// migration with nothing said, instead of failing it with the address named.
func probeClient() *http.Client { return &http.Client{Timeout: 60 * time.Second} }
