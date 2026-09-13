package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/CyLabAcademy/challenge-orchestrator/cmgr"
)

// The challenge-directory commands: what cmgr's own CLI did with a tree, on
// the one host that still has one.
//
// They read and write this build plane and nothing else, and that is the
// whole division. An orchestrator is told what it serves, so what it holds
// is asked of the daemon (cork). What a challenge IS -- its metadata,
// its type, its Dockerfile, whether the tree has drifted from the record --
// is answered here, because here is where the tree is.
//
// Each takes its own flags, as cmgr's did, so `cork-build list --verbose`
// reads the way it always has. The binary's own --verbose sets the log level
// and is parsed before the command name; they do not collide.

// commandFlags makes a command's flag set, reporting usage the way the rest
// of this binary does rather than exiting out from under the caller.
func commandFlags(name, positional string) *flag.FlagSet {
	set := flag.NewFlagSet(name, flag.ContinueOnError)
	set.Usage = func() {
		fmt.Fprintf(set.Output(), "Usage: cork-build %s [<options>] %s\n", name, positional)
		set.PrintDefaults()
	}
	return set
}

// parseCommandFlags separates the two things flag.Parse reports as an error:
// the operator asked for help, and the operator got it wrong. Only the
// second is a failure, and returning the first as one would make
// `cork-build list --help` exit non-zero in a script.
func parseCommandFlags(set *flag.FlagSet, args []string) (int, bool) {
	switch err := set.Parse(args); err {
	case nil:
		return NO_ERROR, true
	case flag.ErrHelp:
		return NO_ERROR, false // already printed by the flag package
	default:
		return USAGE_ERROR, false
	}
}

// listCommand lists what the challenge directory holds, as recorded.
func listCommand(mgr *cmgr.Manager, args []string) int {
	set := commandFlags("list", "")
	verbose := set.Bool("verbose", false, "print each challenge's name as well as its id")
	if code, ok := parseCommandFlags(set, args); !ok {
		return code
	}
	printChallenges(mgr.ListChallenges(), *verbose)
	return NO_ERROR
}

// searchCommand lists the challenges carrying every tag given.
func searchCommand(mgr *cmgr.Manager, args []string) int {
	set := commandFlags("search", "[<tag> ...]")
	verbose := set.Bool("verbose", false, "print each challenge's name as well as its id")
	if code, ok := parseCommandFlags(set, args); !ok {
		return code
	}
	printChallenges(mgr.SearchChallenges(set.Args()), *verbose)
	return NO_ERROR
}

// infoCommand describes the challenges under a path.
func infoCommand(mgr *cmgr.Manager, args []string) int {
	set := commandFlags("info", "[<path>]")
	verbose := set.Bool("verbose", false, "print the description, details and hints too")
	if code, ok := parseCommandFlags(set, args); !ok {
		return code
	}
	if set.NArg() > 1 {
		return usageError("info takes one path at most, and %d were given", set.NArg())
	}
	// The whole tree by default, not the working directory. cmgr's info
	// defaulted to '.' because it was run from inside a challenge directory;
	// cork-build is run by ansible or a CI job from wherever it happens to
	// be, and DetectChanges refuses a path outside CORK_DIR
	// (normalizeDirPath), so '.' would fail for the usual caller.
	path := ""
	if set.NArg() == 1 {
		path = set.Arg(0)
	}

	metadata, err := recordedUnder(mgr, path)
	if err != nil {
		return runtimeError(err)
	}
	for _, cMeta := range metadata {
		fmt.Printf("%s\n", cMeta.Id)
		fmt.Printf("    Name: %s\n", cMeta.Name)
		fmt.Printf("    Challenge Type: %s\n", cMeta.ChallengeType)
		fmt.Printf("    Category: %s\n", cMeta.Category)
		fmt.Printf("    Points: %d\n", cMeta.Points)
		if *verbose {
			fmt.Printf("\n    Description:\n        %s\n", cMeta.Description)
			fmt.Printf("\n    Details:\n        %s\n", cMeta.Details)
			if len(cMeta.Hints) > 0 {
				fmt.Println("\n    Hints")
				for _, hint := range cMeta.Hints {
					fmt.Printf("        - %s\n", hint)
				}
			}
		}
	}
	return NO_ERROR
}

// updateCommand re-scans the challenge directory, records what it finds and
// rebuilds the builds whose source has moved.
//
// It goes no further than this host, and that boundary is worth stating: an
// orchestrator learns of a rebuild only through a hand-over, and a hand-over
// is made per schema, which this command is not given. So an update rebuilds
// and pushes here, and `build` is what puts the result in front of anyone.
// The reading form, --dry-run, carries no such caveat.
func updateCommand(mgr *cmgr.Manager, args []string) int {
	set := commandFlags("update", "[<path>]")
	verbose := set.Bool("verbose", false, "name the unmodified challenges too")
	dryRun := set.Bool("dry-run", false, "report what would change without recording or rebuilding anything")
	pruneOld := set.Bool("prune-old", false, "untag the generation each rebuild displaces from its rollback slot")
	if code, ok := parseCommandFlags(set, args); !ok {
		return code
	}
	if set.NArg() > 1 {
		return usageError("update takes one path at most, and %d were given", set.NArg())
	}
	if *dryRun && *pruneOld {
		return usageError("update --dry-run changes nothing, so there is nothing for --prune-old to untag")
	}

	path := ""
	if set.NArg() == 1 {
		path = set.Arg(0)
	}

	var updates *cmgr.ChallengeUpdates
	if *dryRun {
		updates = mgr.DetectChanges(path)
	} else {
		updates = mgr.UpdateWithOptions(path, cmgr.UpdateOptions{PruneOldImages: *pruneOld})
	}
	printChanges(updates, *verbose)
	if len(updates.Errors) > 0 {
		return RUNTIME_ERROR
	}
	if !*dryRun && len(updates.Updated) > 0 {
		fmt.Printf("\n%d challenge(s) were rebuilt here. An orchestrator hears of a rebuild only\n"+
			"through a hand-over, so run `build` on the schemas that name them.\n", len(updates.Updated))
	}
	return NO_ERROR
}

// dockerfileCommand prints the built-in Dockerfile of a challenge type. It
// is the starting point for a custom challenge, and it is also the thing
// folded into every build identity as the template checksum, so reading it
// is how you see what a type currently means.
func dockerfileCommand(mgr *cmgr.Manager, args []string) int {
	set := commandFlags("dockerfile", "<challenge type>")
	outfile := set.String("output", "", "the `file` to write it to (default: stdout)")
	if code, ok := parseCommandFlags(set, args); !ok {
		return code
	}
	if set.NArg() != 1 {
		return usageError("dockerfile takes one challenge type")
	}

	dockerfile := mgr.GetDockerfile(set.Arg(0))
	if len(dockerfile) == 0 {
		return runtimeError(fmt.Errorf("'%s' is not a challenge type this cork builds for", set.Arg(0)))
	}
	if *outfile == "" {
		fmt.Println(string(dockerfile))
		return NO_ERROR
	}
	if err := os.WriteFile(*outfile, dockerfile, 0o644); err != nil {
		return runtimeError(fmt.Errorf("writing %s: %w", *outfile, err))
	}
	return NO_ERROR
}

// challengeTypeLine matches the type declaration in a problem.md's front
// matter, which is a yaml list item rather than a mapping key.
var challengeTypeLine = regexp.MustCompile(`\n(\s*-\s*type:)\s*(.*)`)

// convertToCustomCommand turns a challenge of a built-in type into a custom
// one: its type's Dockerfile is written into the challenge directory and the
// declared type becomes `custom`, so the tree carries what used to be
// implied and is free to diverge from it.
//
// This changes the challenge's source and therefore its identity. A custom
// challenge's Dockerfile is part of its source checksum, where a built-in
// type's reaches the identity as a template checksum instead, so every build
// of it is a new generation after this -- which is the point: the Dockerfile
// is the tree's to change now.
func convertToCustomCommand(mgr *cmgr.Manager, args []string) int {
	set := commandFlags("convert-to-custom", "<challenge directory>")
	if code, ok := parseCommandFlags(set, args); !ok {
		return code
	}
	if set.NArg() != 1 {
		return usageError("convert-to-custom takes one challenge directory")
	}
	challengeDir := set.Arg(0)

	problemPath := filepath.Join(challengeDir, "problem.md")
	problem, err := os.ReadFile(problemPath)
	if err != nil {
		return runtimeError(fmt.Errorf("reading %s: %w", problemPath, err))
	}
	match := challengeTypeLine.FindSubmatch(problem)
	if match == nil {
		return runtimeError(fmt.Errorf("%s declares no challenge type, so there is none to convert from", problemPath))
	}
	// Trimmed: the pattern runs to the end of the line, so a trailing space
	// or a CRLF file -- which a challenge repo authored on Windows is --
	// would otherwise make the type "remote-make\r" and send it to
	// GetDockerfile, which knows no such type. The 'custom' check below
	// would miss it for the same reason, so an already-converted challenge
	// would be refused with the wrong complaint.
	challengeType := strings.TrimSpace(string(match[2]))
	if challengeType == "custom" {
		return runtimeError(fmt.Errorf("%s is already a custom challenge: its Dockerfile is its own", problemPath))
	}

	// Before anything is read or written: that file would be the
	// challenge's own Dockerfile, and replacing it with the built-in one is
	// not a conversion, it is a loss.
	dockerfilePath := filepath.Join(challengeDir, "Dockerfile")
	if _, err := os.Stat(dockerfilePath); err == nil {
		return runtimeError(fmt.Errorf("%s already exists, and that file would be the challenge's own Dockerfile: this will not write over one", dockerfilePath))
	}

	dockerfile := mgr.GetDockerfile(challengeType)
	if len(dockerfile) == 0 {
		return runtimeError(fmt.Errorf("'%s' is not a challenge type this cork builds for", challengeType))
	}
	if err := os.WriteFile(dockerfilePath, dockerfile, 0o644); err != nil {
		// Whatever reached the disk goes: a half-written Dockerfile is
		// worse than none, and the guard above would refuse to write over
		// it on the retry.
		os.Remove(dockerfilePath)
		return runtimeError(fmt.Errorf("writing %s: %w", dockerfilePath, err))
	}

	// Written whole rather than over: `custom` is shorter than every
	// built-in type name it replaces, and the version this was ported from
	// opened problem.md write-only without truncating, which left the tail
	// of the longer original behind.
	// Through a temporary file and a rename, and written whole rather than
	// over. This rewrites a file that is already there and is somebody's
	// source: a direct write that failed part way would truncate it, and
	// `custom` is shorter than every built-in type name it replaces, so the
	// version this was ported from -- which opened problem.md write-only
	// without truncating -- left the tail of the longer original behind.
	// Either the rename happens or the challenge is untouched.
	converted := challengeTypeLine.ReplaceAll(problem, []byte("\n$1 custom"))
	if err := writeThenRename(problemPath, converted); err != nil {
		// And the Dockerfile comes back out. Half a conversion is a
		// challenge of a built-in type with a stray Dockerfile beside it --
		// which the guard above, refusing to write over one, would then
		// refuse to retry.
		if rmErr := os.Remove(dockerfilePath); rmErr != nil {
			return runtimeError(fmt.Errorf("%w (and %s was left behind: remove it before trying again)", err, dockerfilePath))
		}
		return runtimeError(err)
	}
	fmt.Printf("%s is a custom challenge now: %s holds what '%s' implied.\n"+
		"Its Dockerfile is part of its source, so `update` will see a new generation.\n",
		challengeDir, dockerfilePath, challengeType)
	return NO_ERROR
}

// writeThenRename replaces a file's contents atomically, so a write that
// fails part way leaves the original rather than half of it. The temporary
// goes in the same directory, since a rename across filesystems is not one.
func writeThenRename(path string, content []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".cork-*")
	if err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	tmpPath := tmp.Name()
	// The mode the file already has, since the rename replaces the file and
	// not its contents: CreateTemp makes 0600, and a challenge's problem.md
	// coming back readable only by the user who converted it is not a
	// conversion anyone asked for. A file that is not there yet gets the
	// 0644 an ordinary write would have given it.
	mode := os.FileMode(0o644)
	if info, statErr := os.Stat(path); statErr == nil {
		mode = info.Mode().Perm()
	}
	_, err = tmp.Write(content)
	if chmodErr := tmp.Chmod(mode); err == nil {
		err = chmodErr
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// systemDumpCommand prints what this build plane has built -- its own
// bookkeeping, not what anyone is serving. For that, ask an orchestrator.
func systemDumpCommand(mgr *cmgr.Manager, args []string) int {
	set := commandFlags("system-dump", "[<challenge> ...]")
	summary := set.Bool("summary", false, "print build and instance counts only")
	asJSON := set.Bool("json", false, "print the state as json")
	if code, ok := parseCommandFlags(set, args); !ok {
		return code
	}
	if *summary && *asJSON {
		return usageError("system-dump --summary and --json are two different outputs; ask for one")
	}

	challenges := []cmgr.ChallengeId{}
	for _, id := range set.Args() {
		challenges = append(challenges, cmgr.ChallengeId(id))
	}
	state, err := mgr.DumpState(challenges)
	if err != nil {
		return runtimeError(err)
	}

	switch {
	case *asJSON:
		data, err := json.MarshalIndent(state, "", "    ")
		if err != nil {
			return runtimeError(fmt.Errorf("encoding the state as json: %w", err))
		}
		fmt.Println(string(data))
	case *summary:
		for _, challenge := range state {
			if len(challenge.Builds) == 0 {
				continue
			}
			instances := 0
			for _, build := range challenge.Builds {
				instances += len(build.Instances)
			}
			fmt.Printf("%s: %d builds (%d instances)\n", challenge.Id, len(challenge.Builds), instances)
		}
	default:
		for _, challenge := range state {
			fmt.Printf("%s:\n", challenge.Id)
			for _, build := range challenge.Builds {
				fmt.Printf("    Build ID: %d\n", build.Id)
				for _, instance := range build.Instances {
					fmt.Printf("        %d\n", instance.Id)
				}
			}
		}
	}
	return NO_ERROR
}

func printChallenges(challenges []*cmgr.ChallengeMetadata, verbose bool) {
	for _, challenge := range challenges {
		if verbose {
			fmt.Printf("%s: \"%s\"\n", challenge.Id, challenge.Name)
		} else {
			fmt.Println(challenge.Id)
		}
	}
}

// printChanges names what a scan found, in the buckets DetectChanges sorts
// them into. Unmodified is the one bucket that is usually everything, so it
// is printed only when asked for.
func printChanges(status *cmgr.ChallengeUpdates, verbose bool) {
	for _, section := range []struct {
		title string
		of    []*cmgr.ChallengeMetadata
		quiet bool
	}{
		{"Unmodified", status.Unmodified, true},
		{"Added", status.Added, false},
		{"Refreshed", status.Refreshed, false},
		{"Updated", status.Updated, false},
		{"Stale", status.Stale, false},
		{"Removed", status.Removed, false},
	} {
		if len(section.of) == 0 || (section.quiet && !verbose) {
			continue
		}
		fmt.Printf("%s:\n", section.title)
		for _, md := range section.of {
			fmt.Printf("    %s\n", md.Id)
		}
	}
	if len(status.Errors) > 0 {
		fmt.Println("Errors:")
		for i, err := range status.Errors {
			fmt.Printf("    %d) %s\n", i+1, err)
		}
	}
}

// recordedUnder is the metadata of the challenges under a path. It refuses
// to answer from a tree that has drifted from the database: describing a
// challenge as it was recorded while the directory says something else is
// worse than being told to run `update`.
func recordedUnder(mgr *cmgr.Manager, dir string) ([]*cmgr.ChallengeMetadata, error) {
	cu := mgr.DetectChanges(dir)
	for i, meta := range cu.Unmodified {
		full, err := mgr.GetChallengeMetadata(meta.Id)
		if err != nil {
			cu.Errors = append(cu.Errors, err)
			continue
		}
		cu.Unmodified[i] = full
	}
	if len(cu.Errors) > 0 {
		lines := make([]string, 0, len(cu.Errors))
		for _, err := range cu.Errors {
			lines = append(lines, "  "+err.Error())
		}
		return nil, fmt.Errorf("the challenge directory does not parse:\n%s", joinLines(lines))
	}
	if drifted := len(cu.Added) + len(cu.Updated) + len(cu.Refreshed) + len(cu.Removed); drifted > 0 {
		printChanges(cu, false)
		return nil, fmt.Errorf("%d challenge(s) differ from what is recorded; run `update`", drifted)
	}
	return cu.Unmodified, nil
}
