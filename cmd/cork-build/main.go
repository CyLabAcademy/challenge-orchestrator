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

	exitCode := NO_ERROR
	switch flag.Arg(0) {
	case "build":
		exitCode = buildCommand(mgr, servers, flag.Args()[1:])
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
      name, and hand each challenge over to every --server given. The
      schemas are read here, not by the orchestrator: their flag format and
      seeds decide what is built, and their instance_count travels with the
      hand-over for the orchestrator to converge to. With no --server the
      builds are made and pushed and nothing is handed over.
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

  CMGR_REGISTRY - the registry built images are pushed to, and the one the
      orchestrator's workers pull from; both must name the same registry or
      the hand-over is refused for images the orchestrator cannot find

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
