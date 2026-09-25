package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Local mirrors of the corkd API types (the CLI deliberately does not import
// the cork library — it talks JSON only).

type Schema struct {
	Name       string                        `json:"name"        yaml:"name"`
	FlagFormat string                        `json:"flag_format" yaml:"flag_format"`
	Challenges map[string]BuildSpecification `json:"challenges"  yaml:"challenges"`
}

type BuildSpecification struct {
	Seeds         []int `json:"seeds"          yaml:"seeds"`
	InstanceCount int   `json:"instance_count" yaml:"instance_count"`
}

type UpdateResponse struct {
	Added      []string `json:"added"`
	Refreshed  []string `json:"refreshed"`
	Updated    []string `json:"updated"`
	Stale      []string `json:"stale"`
	Removed    []string `json:"removed"`
	Unmodified []string `json:"unmodified"`
	Errors     []string `json:"errors"`
}

type ChallengeListElement struct {
	Id               string `json:"id"`
	SourceChecksum   uint32 `json:"source_checksum"`
	MetadataChecksum uint32 `json:"metadata_checksum"`
	SolveScript      bool   `json:"solve_script"`
}

type WorkerInfo struct {
	IP        string      `json:"ip"`
	Public    string      `json:"public"`
	Reachable string      `json:"reachable"`
	Load      string      `json:"load"`
	Since     time.Time   `json:"since"`
	Reason    string      `json:"reason"`
	Instances int         `json:"instances"`
	Launch    LaunchStats `json:"launch"`
}

// LaunchStats mirrors the server's summary of what launches on a worker have
// recently cost. Declared here rather than imported for the same reason the
// struct above is: the CLI decodes the API's JSON, and holding its own view
// of it is what keeps a client built against one release readable by the
// next -- an older CLI simply leaves this zero.
type LaunchStats struct {
	Count   int64 `json:"count"`
	Failed  int64 `json:"failed"`
	Samples int64 `json:"samples"`
	P50Ms   int64 `json:"p50_ms"`
	P90Ms   int64 `json:"p90_ms"`
	MaxMs   int64 `json:"max_ms"`
}

func runtimeError(err error) int {
	fmt.Fprintf(os.Stderr, "error: %s\n", err)
	return RUNTIME_ERROR
}

// Deployment ------------------------------------------------------------

func updateCommand(c *client, args []string) int {
	parser := flag.NewFlagSet("update", flag.ExitOnError)
	dryRun := parser.Bool("dry-run", false, "detect changes without applying them")
	verbose := parser.Bool("verbose", false, "also list unmodified challenges")
	pruneOld := parser.Bool("prune-old", false, "remove image generations displaced from rollback retention (daemon and registry)")
	parser.Parse(args)

	if parser.NArg() > 1 {
		parser.Usage()
		return USAGE_ERROR
	}

	path := ""
	if parser.NArg() == 1 {
		// The server resolves relative paths against its own working
		// directory, so anchor the operator's path here first.
		abs, err := filepath.Abs(parser.Arg(0))
		if err != nil {
			return runtimeError(err)
		}
		path = abs
	}

	var resp UpdateResponse
	body := map[string]interface{}{"path": path, "dry_run": *dryRun, "prune_old": *pruneOld}
	if err := c.doJSON("POST", "/update", body, &resp); err != nil {
		return runtimeError(err)
	}

	printSection := func(name string, ids []string) {
		if len(ids) == 0 {
			return
		}
		fmt.Printf("%s:\n", name)
		for _, id := range ids {
			fmt.Printf("  %s\n", id)
		}
	}
	printSection("Added", resp.Added)
	printSection("Refreshed", resp.Refreshed)
	printSection("Updated", resp.Updated)
	printSection("Stale", resp.Stale)
	printSection("Removed", resp.Removed)
	if *verbose {
		printSection("Unmodified", resp.Unmodified)
	}

	if len(resp.Errors) > 0 {
		fmt.Println("Errors:")
		for i, msg := range resp.Errors {
			fmt.Printf("  %d) %s\n", i+1, indentContinuation(msg, "     "))
		}
		return RUNTIME_ERROR
	}
	return NO_ERROR
}

// indentContinuation lines the later lines of a multi-line message up under
// the first. A single error can now report several problems at once --
// validateBuild returns every one it found, joined -- and without this all
// but the first sit flush against the margin, reading as separate output
// rather than as part of the entry they belong to.
func indentContinuation(msg, indent string) string {
	return strings.ReplaceAll(strings.TrimRight(msg, "\n"), "\n", "\n"+indent)
}

func loadSchema(path string) (*Schema, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	schema := new(Schema)
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
	return schema, nil
}

func pushSchemaCommand(c *client, args []string, name string, create bool) int {
	parser := flag.NewFlagSet(name, flag.ExitOnError)
	parser.Parse(args)

	if parser.NArg() != 1 {
		parser.Usage()
		return USAGE_ERROR
	}

	schema, err := loadSchema(parser.Arg(0))
	if err != nil {
		return runtimeError(err)
	}

	path := "/schemas/" + url.PathEscape(schema.Name)
	if create {
		path = "/schemas"
	}
	// Converging a schema builds every missing build synchronously; large
	// schemas take a while and the command waits for completion.
	if err := c.doJSON("POST", path, schema, nil); err != nil {
		return runtimeError(err)
	}
	return NO_ERROR
}

func updateSchemaCommand(c *client, args []string) int {
	return pushSchemaCommand(c, args, "update-schema", false)
}

func addSchemaCommand(c *client, args []string) int {
	return pushSchemaCommand(c, args, "add-schema", true)
}

func removeSchemaCommand(c *client, args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: remove-schema <schema>")
		return USAGE_ERROR
	}
	if err := c.doJSON("DELETE", "/schemas/"+url.PathEscape(args[0]), nil, nil); err != nil {
		return runtimeError(err)
	}
	return NO_ERROR
}

func listSchemasCommand(c *client, args []string) int {
	var schemas []string
	if err := c.doJSON("GET", "/schemas", nil, &schemas); err != nil {
		return runtimeError(err)
	}
	for _, name := range schemas {
		fmt.Println(name)
	}
	return NO_ERROR
}

func showSchemaCommand(c *client, args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: show-schema <schema>")
		return USAGE_ERROR
	}
	body, err := c.do("GET", "/schemas/"+url.PathEscape(args[0]), nil)
	if err != nil {
		return runtimeError(err)
	}
	fmt.Println(prettyJSON(body))
	return NO_ERROR
}

// Challenges and instances ----------------------------------------------

func printChallengeList(challenges []ChallengeListElement, verbose bool) {
	for _, challenge := range challenges {
		if verbose {
			solve := " "
			if challenge.SolveScript {
				solve = "*"
			}
			fmt.Printf("%s %s (source: %08x, metadata: %08x)\n",
				solve, challenge.Id, challenge.SourceChecksum, challenge.MetadataChecksum)
		} else {
			fmt.Println(challenge.Id)
		}
	}
}

func listCommand(c *client, args []string) int {
	parser := flag.NewFlagSet("list", flag.ExitOnError)
	verbose := parser.Bool("verbose", false, "include checksums and solve-script markers")
	parser.Parse(args)

	var challenges []ChallengeListElement
	if err := c.doJSON("GET", "/challenges", nil, &challenges); err != nil {
		return runtimeError(err)
	}
	printChallengeList(challenges, *verbose)
	return NO_ERROR
}

func searchCommand(c *client, args []string) int {
	parser := flag.NewFlagSet("search", flag.ExitOnError)
	verbose := parser.Bool("verbose", false, "include checksums and solve-script markers")
	parser.Parse(args)

	query := url.Values{}
	for _, tag := range parser.Args() {
		query.Add("tags", tag)
	}

	var challenges []ChallengeListElement
	if err := c.doJSON("GET", "/challenges?"+query.Encode(), nil, &challenges); err != nil {
		return runtimeError(err)
	}
	printChallengeList(challenges, *verbose)
	return NO_ERROR
}

func infoCommand(c *client, args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: info <challenge>")
		return USAGE_ERROR
	}
	// Challenge ids contain '/' and the server routes on the raw path, so
	// they are passed through unescaped.
	body, err := c.do("GET", "/challenges/"+args[0], nil)
	if err != nil {
		return runtimeError(err)
	}
	fmt.Println(prettyJSON(body))
	return NO_ERROR
}

func buildCommand(c *client, args []string) int {
	parser := flag.NewFlagSet("build", flag.ExitOnError)
	flagFormat := parser.String("flag-format", "flag{%s}", "the flag format for the built challenges")
	parser.Parse(args)

	if parser.NArg() < 2 {
		fmt.Fprintln(os.Stderr, "usage: build [--flag-format <format>] <challenge> <seed> [<seed> ...]")
		return USAGE_ERROR
	}

	seeds := []int{}
	for _, seedStr := range parser.Args()[1:] {
		seed, err := strconv.Atoi(seedStr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: could not interpret '%s' as a seed: %s\n", seedStr, err)
			return USAGE_ERROR
		}
		seeds = append(seeds, seed)
	}

	var builds []struct {
		Id int `json:"id"`
	}
	body := map[string]interface{}{"flag_format": *flagFormat, "seeds": seeds}
	if err := c.doJSON("POST", "/challenges/"+parser.Arg(0), body, &builds); err != nil {
		return runtimeError(err)
	}

	fmt.Println("Build IDs:")
	for _, build := range builds {
		fmt.Printf("  %d\n", build.Id)
	}
	return NO_ERROR
}

func destroyCommand(c *client, args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: destroy <build>")
		return USAGE_ERROR
	}
	if _, err := strconv.Atoi(args[0]); err != nil {
		fmt.Fprintf(os.Stderr, "error: could not interpret '%s' as a build id: %s\n", args[0], err)
		return USAGE_ERROR
	}
	if err := c.doJSON("DELETE", "/builds/"+args[0], nil, nil); err != nil {
		return runtimeError(err)
	}
	return NO_ERROR
}

func startCommand(c *client, args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: start <build>")
		return USAGE_ERROR
	}
	if _, err := strconv.Atoi(args[0]); err != nil {
		fmt.Fprintf(os.Stderr, "error: could not interpret '%s' as a build id: %s\n", args[0], err)
		return USAGE_ERROR
	}
	body, err := c.do("POST", "/builds/"+args[0], nil)
	if err != nil {
		return runtimeError(err)
	}
	fmt.Println(prettyJSON(body))
	return NO_ERROR
}

func stopCommand(c *client, args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: stop <instance>")
		return USAGE_ERROR
	}
	if _, err := strconv.Atoi(args[0]); err != nil {
		fmt.Fprintf(os.Stderr, "error: could not interpret '%s' as an instance id: %s\n", args[0], err)
		return USAGE_ERROR
	}
	if err := c.doJSON("DELETE", "/instances/"+args[0], nil, nil); err != nil {
		return runtimeError(err)
	}
	return NO_ERROR
}

func systemDumpCommand(c *client, args []string) int {
	body, err := c.do("GET", "/state", nil)
	if err != nil {
		return runtimeError(err)
	}
	fmt.Println(prettyJSON(body))
	return NO_ERROR
}

// Workers ----------------------------------------------------------------

func workerAddCommand(c *client, args []string) int {
	if len(args) < 1 || len(args) > 2 {
		fmt.Fprintln(os.Stderr, "usage: worker-add <ip> [<public address>]")
		return USAGE_ERROR
	}
	body := map[string]string{"ip": args[0]}
	if len(args) == 2 {
		body["public"] = args[1]
	}
	if err := c.doJSON("POST", "/workers", body, nil); err != nil {
		return runtimeError(err)
	}
	return NO_ERROR
}

func workerRemoveCommand(c *client, args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: worker-remove <ip>")
		return USAGE_ERROR
	}
	if err := c.doJSON("DELETE", "/workers/"+url.PathEscape(args[0]), nil, nil); err != nil {
		return runtimeError(err)
	}
	return NO_ERROR
}

func workerDownCommand(c *client, args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: worker-down <ip>")
		return USAGE_ERROR
	}
	body := map[string]string{"health": "down"}
	if err := c.doJSON("PATCH", "/workers/"+url.PathEscape(args[0]), body, nil); err != nil {
		return runtimeError(err)
	}
	return NO_ERROR
}

func workerListCommand(c *client, args []string) int {
	var workers []WorkerInfo
	if err := c.doJSON("GET", "/workers", nil, &workers); err != nil {
		return runtimeError(err)
	}
	for _, worker := range workers {
		public := worker.Public
		if public == "" {
			public = worker.IP
		}
		// How long it has been in this state matters as much as the state: a
		// worker unresponsive for eight seconds is a daemon restarting, one
		// unresponsive for an hour is a box nobody has looked at.
		state := worker.Reachable
		if !worker.Since.IsZero() {
			state += fmt.Sprintf(" %s", time.Since(worker.Since).Round(time.Second))
		}
		fmt.Printf("%-15s  public=%-30s  %-22s  load=%-10s  %d instances%s\n",
			worker.IP, public, state, worker.Load, worker.Instances, launchSummary(worker.Launch))
		if worker.Reason != "" {
			fmt.Printf("%-15s  %s\n", "", worker.Reason)
		}
	}
	return NO_ERROR
}

// launchSummary renders what launches on a worker have recently cost. The
// quantiles come from a bounded ring on the daemon, so they describe its last
// couple of hundred successful launches, while n is every launch the worker
// has taken since corkd started -- which is why a worker that has gone quiet
// can show a large n against quantiles measured some time ago.
//
// Nothing is printed for a worker that has taken no launches: a row of zeroes
// reads like a fast worker rather than an unused one. A worker taking
// launches and finishing none says that instead, for the same reason.
func launchSummary(s LaunchStats) string {
	if s.Count == 0 {
		return ""
	}
	failed := ""
	if s.Failed > 0 {
		failed = fmt.Sprintf(" failed=%d", s.Failed)
	}
	if s.Samples == 0 {
		return fmt.Sprintf("  launch=none succeeded n=%d%s", s.Count, failed)
	}
	return fmt.Sprintf("  launch=p50 %s/p90 %s/max %s n=%d%s",
		shortMillis(s.P50Ms), shortMillis(s.P90Ms), shortMillis(s.MaxMs), s.Count, failed)
}

// shortMillis prints a duration the way an operator scanning a column reads
// it: whole milliseconds under a second, then one decimal of seconds.
func shortMillis(ms int64) string {
	if ms < 1000 {
		return fmt.Sprintf("%dms", ms)
	}
	return fmt.Sprintf("%.1fs", float64(ms)/1000)
}

// Other -------------------------------------------------------------------

func versionCommand(c *client, args []string) int {
	fmt.Printf("client: %s\n", clientVersion())

	var resp struct {
		Version string `json:"version"`
	}
	if err := c.doJSON("GET", "/version", nil, &resp); err != nil {
		return runtimeError(err)
	}
	fmt.Printf("server: %s (%s)\n", resp.Version, strings.TrimRight(c.base, "/"))
	return NO_ERROR
}
