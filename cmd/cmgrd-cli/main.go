package main

import (
	"flag"
	"fmt"
	"os"
)

const (
	NO_ERROR      = 0
	RUNTIME_ERROR = -1
	USAGE_ERROR   = -2
)

const serverEnv = "CMGRD_SERVER"
const defaultServer = "http://127.0.0.1:4200"

// Set at build time via -ldflags "-X main.version=$(git describe --tags)".
var version string

func clientVersion() string {
	if version != "" {
		return version
	}
	return "unknown"
}

func main() {
	server := os.Getenv(serverEnv)
	if server == "" {
		server = defaultServer
	}
	flag.StringVar(&server, "server", server, "base URL of the cmgrd server")
	help := flag.Bool("help", false, "display usage information")
	flag.Parse()

	if *help || flag.NArg() == 0 {
		printUsage()
		if *help {
			os.Exit(NO_ERROR)
		}
		os.Exit(USAGE_ERROR)
	}

	c := newClient(server)
	cmdArgs := flag.Args()[1:]

	var exitCode int
	switch flag.Arg(0) {
	case "update":
		exitCode = updateCommand(c, cmdArgs)
	case "update-schema":
		exitCode = updateSchemaCommand(c, cmdArgs)
	case "add-schema":
		exitCode = addSchemaCommand(c, cmdArgs)
	case "remove-schema":
		exitCode = removeSchemaCommand(c, cmdArgs)
	case "list-schemas":
		exitCode = listSchemasCommand(c, cmdArgs)
	case "show-schema":
		exitCode = showSchemaCommand(c, cmdArgs)
	case "list":
		exitCode = listCommand(c, cmdArgs)
	case "search":
		exitCode = searchCommand(c, cmdArgs)
	case "info":
		exitCode = infoCommand(c, cmdArgs)
	case "build":
		exitCode = buildCommand(c, cmdArgs)
	case "destroy":
		exitCode = destroyCommand(c, cmdArgs)
	case "start":
		exitCode = startCommand(c, cmdArgs)
	case "stop":
		exitCode = stopCommand(c, cmdArgs)
	case "system-dump":
		exitCode = systemDumpCommand(c, cmdArgs)
	case "worker-add":
		exitCode = workerAddCommand(c, cmdArgs)
	case "worker-remove":
		exitCode = workerRemoveCommand(c, cmdArgs)
	case "worker-down":
		exitCode = workerDownCommand(c, cmdArgs)
	case "worker-list":
		exitCode = workerListCommand(c, cmdArgs)
	case "artifacts":
		exitCode = artifactsCommand(c, cmdArgs)
	case "version":
		exitCode = versionCommand(c, cmdArgs)
	case "help":
		printUsage()
		exitCode = NO_ERROR
	default:
		fmt.Fprintf(os.Stderr, "error: unknown command '%s'\n", flag.Arg(0))
		printUsage()
		exitCode = USAGE_ERROR
	}

	os.Exit(exitCode)
}

func printUsage() {
	fmt.Printf(`
Usage: %s [--server <url>] <command> [<args>]

A thin HTTP client for cmgrd: every command is an API call against the
server; nothing touches the database, docker, or the registry directly.

Deployment:
  update [--dry-run] [--verbose] [--prune-old] [<dir>]
      re-scan the challenge directory on the server (rebuilding changed
      challenges) and print the resulting changes; <dir> must be inside the
      server's CMGR_DIR and defaults to all of it; --prune-old additionally
      removes the image generation each rebuild displaces from rollback
      retention, on the build daemon and in the registry
  update-schema <schema file>
      create or update the schema defined in the given yaml/json file and
      converge its builds/instances
  add-schema <schema file>
      like update-schema, but errors if the schema already exists
  remove-schema <schema>
      delete the schema and destroy its builds/instances
  list-schemas
  show-schema <schema>
      print the schema's full nested challenge/build/instance state

Challenges and instances:
  list [--verbose]
  search [--verbose] <tag> [<tag> ...]
  info <challenge>
  build [--flag-format <format>] <challenge> <seed> [<seed> ...]
  destroy <build>
  start <build>
  stop <instance>
  system-dump
      print the full nested state of every challenge

Workers:
  worker-add <ip> [<public address>]
      register a worker (or rebuild the connection of a down one); the
      optional public address is what players are given for its instances
  worker-remove <ip>
      purge the worker and all of its instance records
  worker-down <ip>
      mark the worker down, taking it out of placement but keeping its records
  worker-list

Other:
  artifacts <build> [<output file>]
      download the build's artifacts tarball (default: <build>.tar.gz)
  version
      print client and server versions

The server defaults to %s and can also be set via the
%s environment variable.
`, os.Args[0], defaultServer, serverEnv)
}
