package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/CyLabAcademy/challenge-orchestrator/cmgr"
)

// takenRequest is what an orchestrator saw.
type takenRequest struct {
	method string
	path   string
	query  string
	body   []byte
}

// orchestratorServing stands in for a corkd serving the schemas named, and
// records what reached it. It answers as corkd does, which is the point of
// it: an empty list for a schema it does not have, not a 404.
func orchestratorServing(t *testing.T, schemas ...string) (string, *[]takenRequest) {
	t.Helper()
	serves := map[string]bool{}
	for _, s := range schemas {
		serves[s] = true
	}
	seen := &[]takenRequest{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/schemas/")
		body, _ := io.ReadAll(r.Body)
		*seen = append(*seen, takenRequest{r.Method, r.URL.Path, r.URL.RawQuery, body})
		switch r.Method {
		case http.MethodGet:
			// As corkd answers: GET /schemas/<name> is a query over the
			// builds table, so a schema it has never heard of is an empty
			// list and a 200, not a 404. A fake that 404s there would have
			// taught this test the wrong contract.
			if serves[name] {
				w.Write([]byte(`[{"id":"a/one","builds":[{"id":1}]}]`))
			} else {
				w.Write([]byte("[]"))
			}
		case http.MethodPost:
			// update-schema, as corkd answers it: 204 and no body.
			w.WriteHeader(http.StatusNoContent)
		case http.MethodDelete:
			// As corkd answers, for a schema it serves and one it has never
			// heard of alike: DeleteSchema over no rows removes nothing and
			// fails at nothing, so both are 204. A fake that 404d on the
			// second would have taught this test that a removal aimed at
			// the wrong orchestrator announces itself, and it does not --
			// which is why removalTarget asks where the schema is rather
			// than trusting the file.
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(server.Close)
	return server.URL, seen
}

// closedAddress is an address nothing listens on: a server started and then
// stopped, so a connection there is refused at once rather than waiting out
// a timeout on an address that may simply be unroutable.
func closedAddress(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	address := server.URL
	server.Close()
	return address
}

// A schema is somewhere or it is not, and the orchestrator says which in the
// state it returns rather than in its status: an empty list is "not here".
// A status that is neither 200 nor 404 is an answer a migration cannot act
// on, and is not read as "no".
func TestServesSchema(t *testing.T) {
	address, _ := orchestratorServing(t, "spring")
	if has, err := servesSchema(address, "spring"); err != nil || !has {
		t.Errorf("a schema the orchestrator serves: %v, %v", has, err)
	}
	if has, err := servesSchema(address, "autumn"); err != nil || has {
		t.Errorf("a schema it does not serve: %v, %v", has, err)
	}

	// A 500 is not "no": migrating on the strength of it would release a
	// schema from an orchestrator that never confirmed it had one.
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(broken.Close)
	if _, err := servesSchema(broken.URL, "spring"); err == nil {
		t.Error("a 500 was taken for an answer")
	}
	if _, err := servesSchema(closedAddress(t), "spring"); err == nil {
		t.Error("an orchestrator that could not be reached was taken for an answer")
	}
}

// The wire difference between a removal and a migration is one query
// parameter, and it decides whether the images survive. Worth asserting
// exactly, because getting it backwards is silent until an event cannot
// pull.
func TestDeleteSchemaOn(t *testing.T) {
	address, seen := orchestratorServing(t, "spring")

	if err := deleteSchemaOn(address, "spring", true); err != nil {
		t.Fatalf("removing: %s", err)
	}
	last := (*seen)[len(*seen)-1]
	if last.method != http.MethodDelete || last.path != "/schemas/spring" {
		t.Errorf("removal sent %s %s", last.method, last.path)
	}
	if last.query != "" {
		t.Errorf("a removal carried %q: it must retire the tags, which is the default", last.query)
	}

	if err := deleteSchemaOn(address, "spring", false); err != nil {
		t.Fatalf("releasing: %s", err)
	}
	last = (*seen)[len(*seen)-1]
	if last.query != "retire=false" {
		t.Errorf("a migration's release carried %q, want retire=false: without it the orchestrator retires the tags the next one is about to serve", last.query)
	}

	// A schema the orchestrator has never heard of is removed just as
	// successfully, because DeleteSchema over no rows removes nothing and
	// fails at nothing. Asserted rather than assumed: it is the whole reason
	// removalTarget asks where a schema is served instead of trusting the
	// destination its file names.
	if err := deleteSchemaOn(address, "autumn", true); err != nil {
		t.Errorf("a removal aimed at an orchestrator with no such schema was refused: %s", err)
	}

	// A refusal is reported with what the orchestrator said.
	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("the database is locked"))
	}))
	t.Cleanup(refusing.Close)
	err := deleteSchemaOn(refusing.URL, "spring", true)
	if err == nil {
		t.Fatal("an orchestrator that refused the removal was reported as having done it")
	}
	if !strings.Contains(err.Error(), "the database is locked") {
		t.Errorf("the refusal does not quote what the orchestrator said: %s", err)
	}
	if err := deleteSchemaOn(closedAddress(t), "spring", true); err == nil {
		t.Error("removing from an orchestrator that could not be reached was reported as done")
	}
}

// A removal goes to the orchestrator serving the schema, not to the one its
// file names. They differ exactly when a destination was edited and not
// migrated -- and since a DELETE for a schema a daemon does not have is a
// 204, aiming at the file's destination would report a removal that removed
// nothing while the event kept running.
func TestRemovalTarget(t *testing.T) {
	library, _ := orchestratorServing(t, "year-round")
	event, _ := orchestratorServing(t, "spring")
	dests, err := loadDestinations(writeDestinations(t, "library: "+library+"\nevent: "+event+"\n"))
	if err != nil {
		t.Fatal(err)
	}

	name, address, err := removalTarget("spring", nil, dests)
	if err != nil || name != "event" || address != event {
		t.Errorf("a schema served by 'event' is removed from %q (%q), %v", name, address, err)
	}
	// Served nowhere is not an error: this build plane's own rows may still
	// be there, and dropping them is what is left to do.
	name, address, err = removalTarget("winter", nil, dests)
	if err != nil || address != "" {
		t.Errorf("a schema nobody serves: %q (%q), %v", name, address, err)
	}
	// --server overrides the lookup entirely, which is the way out when a
	// destination cannot be asked.
	if name, address, err := removalTarget("spring", []string{"http://only:4200"}, dests); err != nil ||
		name != "--server" || address != "http://only:4200" {
		t.Errorf("--server did not override the lookup: %q (%q), %v", name, address, err)
	}
	// And when one cannot be asked, the refusal says --server is the way on.
	unreachable, err := loadDestinations(writeDestinations(t, "event: "+event+"\ndead: "+closedAddress(t)+"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := removalTarget("spring", nil, unreachable); err == nil {
		t.Error("a destination that could not be asked was treated as not serving the schema")
	} else if !strings.Contains(err.Error(), "--server") {
		t.Errorf("the refusal does not say how to proceed: %s", err)
	}

	// "Nowhere is serving it" is a conclusion drawn from asking, and with
	// nothing configured there is nobody to ask. Answering it the same way
	// would drop this build plane's rows for a schema an orchestrator no
	// one named is still running.
	none, err := loadDestinations("")
	if err != nil {
		t.Fatal(err)
	}
	if _, address, err := removalTarget("spring", nil, none); err == nil {
		t.Errorf("with no destinations configured a removal was aimed at %q rather than refused", address)
	} else if !strings.Contains(err.Error(), DESTINATIONS_ENV) {
		t.Errorf("the refusal does not name %s: %s", DESTINATIONS_ENV, err)
	}
}

// migrate-schema with nothing configured is refused, and refused before it
// reaches the database or the network -- which is what handing it no manager
// at all proves. Its own message rather than resolve's, since resolve offers
// --server and a migration moves a schema between configured destinations.
func TestMigrateSchemaNeedsDestinations(t *testing.T) {
	file := writeFile(t, "spring.yaml", "name: spring\nflag_format: flag{%s}\nchallenges:\n  test/one:\n    seeds: [1]\n    instance_count: -1\n")
	none, err := loadDestinations("")
	if err != nil {
		t.Fatal(err)
	}
	if code := migrateSchemaCommand(nil, nil, none, []string{file}); code == NO_ERROR {
		t.Error("migrate-schema ran with no destinations configured; there is nothing to migrate between")
	}
	// And --server is refused whatever else is true: a migration is between
	// destinations, so an address given directly names no 'from'.
	if code := migrateSchemaCommand(nil, []string{"http://only:4200"}, none, []string{file}); code != USAGE_ERROR {
		t.Errorf("migrate-schema with --server answered %d, want a usage error", code)
	}
}

// Handing builds over records them; converging is what puts them in service.
// It goes to POST /schemas/<name> -- update-schema, not add-schema, because
// the hand-over has already made the schema exist as far as the daemon is
// concerned -- and it carries the schema as written, instance counts and all.
func TestConvergeOn(t *testing.T) {
	address, seen := orchestratorServing(t, "spring")
	schema := routedSchema("spring", "event", "flag{%s}", map[cmgr.ChallengeId][]int{"a/one": {1}})

	if err := convergeOn(address, schema); err != nil {
		t.Fatalf("converging: %s", err)
	}
	last := (*seen)[len(*seen)-1]
	if last.method != http.MethodPost || last.path != "/schemas/spring" {
		t.Errorf("the converge sent %s %s, want POST /schemas/spring: POST /schemas would be add-schema, which is refused for a schema the hand-over has already brought into being",
			last.method, last.path)
	}
	// What it sent is the schema as written, not the on-demand one the
	// build plane converged its own database with: the counts are the whole
	// reason the orchestrator is being asked to converge.
	var sent cmgr.Schema
	if err := json.Unmarshal(last.body, &sent); err != nil {
		t.Fatalf("the converge body is not a schema: %s (%q)", err, last.body)
	}
	if sent.Name != "spring" || sent.FlagFormat != "flag{%s}" {
		t.Errorf("the converge sent name %q, flag format %q", sent.Name, sent.FlagFormat)
	}
	if got := sent.Challenges["a/one"].InstanceCount; got != cmgr.DYNAMIC_INSTANCES {
		t.Errorf("the converge sent instance count %d, want the schema's %d", got, cmgr.DYNAMIC_INSTANCES)
	}

	// A refusal is reported rather than swallowed: a converge that did not
	// happen is a schema handed over and serving nothing.
	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte("build 7 of 'a/one' has not been handed over"))
	}))
	t.Cleanup(refusing.Close)
	err := convergeOn(refusing.URL, schema)
	if err == nil {
		t.Fatal("an orchestrator that refused the converge was reported as having done it")
	}
	if !strings.Contains(err.Error(), "has not been handed over") {
		t.Errorf("the refusal does not quote what the orchestrator said: %s", err)
	}
	if err := convergeOn(closedAddress(t), schema); err == nil {
		t.Error("converging on an orchestrator that could not be reached was reported as done")
	}
}

// add-schema and update-schema do the same work; the only thing that
// separates them is whether this plane is expected to have built the schema
// before. add on a name already in use is usually a second event written
// against the first one's schema; update on a name that is not is usually a
// typo. build asks neither question.
func TestSchemaGuard(t *testing.T) {
	known := []string{"year-round", "spring"}

	if err := schemaGuard("autumn", known, false); err != nil {
		t.Errorf("add-schema for a schema this plane has not built: %s", err)
	}
	if err := schemaGuard("spring", known, true); err != nil {
		t.Errorf("update-schema for a schema this plane has built: %s", err)
	}

	err := schemaGuard("spring", known, false)
	if err == nil {
		t.Fatal("add-schema was allowed for a schema this plane has already built")
	}
	if !strings.Contains(err.Error(), "update-schema") {
		t.Errorf("the refusal does not name the command that does want an existing schema: %s", err)
	}
	err = schemaGuard("autumn", known, true)
	if err == nil {
		t.Fatal("update-schema was allowed for a schema this plane has never built")
	}
	if !strings.Contains(err.Error(), "add-schema") {
		t.Errorf("the refusal does not name the command that creates one: %s", err)
	}
}

// remove-schema took a schema file until recently and takes a name now, so
// a caller that has not been updated is the likeliest way to reach it wrong.
// Named as the mistake it is, and before the name is checked at all: a path
// contains a slash, and being told a slash cannot address a schema over HTTP
// is true and no help. No manager is needed to get that far, which is what
// passing none proves.
func TestRemoveSchemaRejectsASchemaFile(t *testing.T) {
	for _, arg := range []string{"spring.yaml", "spring.yml", "spring.json", "/etc/cork/schemas/spring.yaml"} {
		if code := removeSchemaCommand(nil, nil, nil, []string{arg}); code != USAGE_ERROR {
			t.Errorf("remove-schema %q answered %d, want a usage error naming it as a file", arg, code)
		}
	}
	// A name is not a file just for having a dot in it: this one gets past
	// the check and is stopped further on, for having nowhere to ask.
	none, err := loadDestinations("")
	if err != nil {
		t.Fatal(err)
	}
	if code := removeSchemaCommand(nil, nil, none, []string{"spring.2026"}); code == USAGE_ERROR {
		t.Error("a schema name with a dot in it was refused as a file")
	}
}

// A migration has to find where the schema is now, and the schema files are
// the record of where it is going -- so the orchestrators are asked. Exactly
// one may have it.
func TestWhereSchemaLives(t *testing.T) {
	library, _ := orchestratorServing(t, "year-round")
	event, _ := orchestratorServing(t, "spring")
	dests, err := loadDestinations(writeDestinations(t, "library: "+library+"\nevent: "+event+"\n"))
	if err != nil {
		t.Fatal(err)
	}

	name, address, err := whereSchemaLives("spring", dests)
	if err != nil || name != "event" || address != event {
		t.Errorf("spring lives at %q (%q), %v", name, address, err)
	}
	// Served nowhere is not an error: the caller decides what that means.
	name, address, err = whereSchemaLives("winter", dests)
	if err != nil || name != "" || address != "" {
		t.Errorf("a schema nobody serves: %q (%q), %v", name, address, err)
	}

	// Two orchestrators serving one schema is the state exclusivity exists
	// to prevent; it is reported rather than guessed between.
	both, _ := orchestratorServing(t, "spring")
	twice, err := loadDestinations(writeDestinations(t, "event: "+event+"\nspare: "+both+"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := whereSchemaLives("spring", twice); err == nil {
		t.Error("a schema served by two orchestrators was migrated from one of them at random")
	} else if !strings.Contains(err.Error(), "spring") {
		t.Errorf("the refusal does not name the schema: %s", err)
	}

	// An orchestrator that cannot be asked stops the migration: releasing a
	// schema from one while another's state is unknown is how both end up
	// serving it, or neither.
	unreachable, err := loadDestinations(writeDestinations(t, "event: "+event+"\ndead: "+closedAddress(t)+"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := whereSchemaLives("spring", unreachable); err == nil {
		t.Error("an unreachable destination was treated as not serving the schema")
	}
}
