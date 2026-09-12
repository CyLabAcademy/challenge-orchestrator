// cork-build is a cork deployment's build plane: the half of cmgrd that
// CMGR_BUILD_PLANE=external leaves out (issue #18). It reads the schema files
// an event is defined by, scans the challenge directory, builds and pushes
// every build those schemas name, and hands the finished builds to the
// orchestrator over PUT /challenges/<id>.
//
// It is cmgr's own build path, run somewhere else: the same scan, the same
// converge, the same registry. What it does not do is run anything. Every
// schema is converged here with its builds on demand, which launches no
// instance, and the count the schema really asks for travels in the
// hand-over, for the orchestrator to converge to.
//
// Its database is bookkeeping, not a source of truth: the orchestrator's is
// the one that matters, and the registry is where the images this made are
// found. Keeping it between runs only makes them faster (a challenge whose
// source has not moved is not rebuilt); losing it costs a re-derivation, in
// which every image already in the registry is adopted rather than built.
package main

import (
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/CyLabAcademy/challenge-orchestrator/cmgr"
)

const (
	NO_ERROR      = 0
	RUNTIME_ERROR = -1
	USAGE_ERROR   = -2
)

// Set at build time from ci/version.sh, as for the daemon and the CLI. The
// orchestrator recomputes every build's identity with its own copy of the
// challenge templates, so a builder and a daemon of different versions can
// disagree on what a build is; a hand-over says so before it is sent.
var version string

func buildVersion() string {
	if version != "" {
		return version
	}
	return "unknown"
}

func runtimeError(err error) int {
	fmt.Fprintf(os.Stderr, "error: %s\n", err)
	return RUNTIME_ERROR
}

func usageError(format string, args ...any) int {
	fmt.Fprintf(os.Stderr, "error: %s\n", fmt.Sprintf(format, args...))
	return USAGE_ERROR
}

// serverList collects a repeated --server: one deployment can have more than
// one orchestrator, and every one of them wants the same builds.
type serverList []string

func (l *serverList) String() string { return strings.Join(*l, ", ") }

func (l *serverList) Set(value string) error {
	if value == "" {
		return fmt.Errorf("empty server address")
	}
	// Checked here, where it costs a usage error, rather than at the
	// hand-over, where it would cost an event's worth of building first:
	// every later use of this is a URL, and checkServer only warns about a
	// server it cannot reach, so an address no request can be made against
	// would otherwise surface as every challenge failing at the end.
	parsed, err := url.Parse(value)
	if err != nil {
		return fmt.Errorf("'%s' is not an address: %w", value, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("'%s' is not an http(s) address", value)
	}
	if parsed.Host == "" {
		return fmt.Errorf("'%s' names no host", value)
	}
	*l = append(*l, strings.TrimRight(value, "/"))
	return nil
}

func main() {
	var servers serverList
	flag.Var(&servers, "server", "orchestrator to hand the builds to; repeat for more than one, omit to build and push without handing anything over")
	dir := flag.String("dir", "", "challenge directory, overriding "+cmgr.DIR_ENV)
	verbose := flag.Bool("verbose", false, "log every docker and database step")
	help := flag.Bool("help", false, "display usage information")
	showVersion := flag.Bool("version", false, "display version information and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("Version: %s\n", buildVersion())
		os.Exit(NO_ERROR)
	}
	if *help || flag.NArg() == 0 {
		printUsage()
		if *help {
			os.Exit(NO_ERROR)
		}
		os.Exit(USAGE_ERROR)
	}
	if *dir != "" {
		// The manager reads the directory from the environment, as the
		// daemon does. The flag is for a runner that has the tree in its
		// workspace rather than in its environment.
		if err := os.Setenv(cmgr.DIR_ENV, *dir); err != nil {
			os.Exit(runtimeError(err))
		}
	}

	logLevel := cmgr.INFO
	if *verbose {
		logLevel = cmgr.DEBUG
	}
	// A build plane, not an orchestrator: it pushes to the shared registry
	// and never takes anything back out of it. Its own database is
	// bookkeeping, and what a schema here stops naming may still be what an
	// orchestrator is serving.
	mgr := cmgr.NewManager(logLevel, cmgr.AsBuildPlane())
	if mgr == nil {
		fmt.Fprintln(os.Stderr, "error: could not initialize cmgr")
		os.Exit(RUNTIME_ERROR)
	}
	// The one setting this binary cannot honour: it IS the build plane, and
	// an external one has neither the docker daemon nor the tree it needs.
	if mgr.BuildPlane() != cmgr.BuildPlaneLocal {
		fmt.Fprintf(os.Stderr, "error: %s=%s, but cork-build is the build plane itself: it builds from %s on the docker daemon DOCKER_HOST names\n",
			cmgr.BUILD_PLANE_ENV, mgr.BuildPlane(), cmgr.DIR_ENV)
		os.Exit(RUNTIME_ERROR)
	}

	// Where the destination names schemas use point: deployment topology,
	// written by the same ansible that stands the orchestrators up, so a
	// rebuilt build plane gets its routing back the way it gets everything
	// else. Read before any command, since all three route.
	dests, err := loadDestinations(os.Getenv(DESTINATIONS_ENV))
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %s\n", err)
		os.Exit(RUNTIME_ERROR)
	}

	exitCode := NO_ERROR
	switch flag.Arg(0) {
	case "build":
		exitCode = buildCommand(mgr, servers, dests, flag.Args()[1:])
	case "remove-schema":
		exitCode = removeSchemaCommand(mgr, servers, dests, flag.Args()[1:])
	case "migrate-schema":
		exitCode = migrateSchemaCommand(mgr, servers, dests, flag.Args()[1:])
	case "destinations":
		exitCode = destinationsCommand(dests)
	case "pins":
		exitCode = pinsCommand(mgr)
	case "version":
		fmt.Printf("Version: %s\n", buildVersion())
	default:
		exitCode = usageError("unknown command '%s'", flag.Arg(0))
		printUsage()
	}
	os.Exit(exitCode)
}

func printUsage() {
	fmt.Printf(`
Usage: %s [<options>] <command> [<args>]

cork's build plane: it builds what the schema files name and hands the
finished builds to an orchestrator running with CMGR_BUILD_PLANE=external,
which builds nothing itself.

Commands:
  build <schema file> [<schema file> ...]
      scan the challenge directory, build and push every build the schemas
      name, and hand each challenge over to the orchestrator its schema is
      for. The schemas are read here, not by the orchestrator: their flag
      format and seeds decide what is built, their destination decides which
      orchestrator serves them, and their instance_count travels with the
      hand-over. Each schema is then converged on the orchestrator that took
      it, once every challenge of it has arrived, so this is the whole deploy
      rather than its first half. --server overrides the destinations and
      sends everything to the address given.
      Build the schemas that share a challenge in one run: one
      content-addressed tag cannot be served by two orchestrators, and that
      is checked across the schemas of a run.
  remove-schema <schema name>
      take the schema out of service: the orchestrator serving it drops its
      builds and retires their images from the registry, and this build
      plane drops its own rows for them. Destructive on both sides -- a
      later build of the same schema builds and pushes it again. A name and
      not a file, since nothing else about the schema survives this: which
      orchestrator has it is found by asking them, and --server names one
      directly.
  migrate-schema <schema file>
      move the schema to the destination its file now names. Its images stay
      in the registry and are adopted by the orchestrator taking it: a
      build's identity does not depend on which orchestrator serves it, so a
      migration rebuilds nothing and takes seconds, and the orchestrator
      taking it is converged, so the move finishes rather than half of it.
  destinations
      list the destination names configured and the orchestrators they
      stand for.
  pins
      re-resolve every base image the challenge directory names to the
      digest the registry serves now, and write CMGR_BASE_PINS. Nothing is
      rebuilt on account of it: a moved base reaches a challenge the next
      time that challenge is built, so refresh before a build, not after.
  version

Options:
  --server   orchestrator to hand the builds to; repeat for more than one
  --dir      challenge directory, overriding CMGR_DIR
  --verbose  log every docker and database step
  --help
  --version

Environment, all as cmgrd reads them (this is cmgrd's build path):
  CMGR_DIR - the challenge directory to build from

  CORK_DESTINATIONS - a yaml file mapping the destination names schemas use
      to the orchestrators they stand for ("library: https://host:4200").
      A schema naming no destination means the only one configured, and is
      refused once there is more than one -- so a single-orchestrator
      deployment need say nothing, and an event cannot land on the wrong
      orchestrator because a line was forgotten.

  CMGR_REGISTRY - required. The registry built images are pushed to, and
      the one the orchestrator's workers pull from; both must name the same
      registry or the hand-over is refused for images the orchestrator
      cannot find

  CMGR_DB - this builder's own database (defaults to 'cmgr.db'). It is
      bookkeeping and may be thrown away: keeping it between runs only
      saves rebuilding what has not changed

  CMGR_ARTIFACT_DIR - where built artifact bundles are kept until they are
      handed over (defaults to '.')

  CMGR_BASE_PINS - the base image pin file, defaulting to
      <CMGR_DIR>/.base-pins.json. The pins are part of every build's
      identity, so the orchestrator is told the fingerprint they were built
      under and recomputes with it

  CMGR_PURGE_AFTER_PUSH - drop this host's copy of an image once it is in
      the registry, which is what a builder wants: nothing runs here

  DOCKER_HOST and the rest of docker's own variables - the daemon that
      builds. See https://docs.docker.com/engine/reference/commandline/cli/

Exit status is non-zero if anything failed to build or to be handed over.
`, os.Args[0])
}
