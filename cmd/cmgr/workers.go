package main

import (
	"flag"
	"fmt"

	"github.com/picoCTF/cmgr/cmgr"
)

func addWorker(mgr *cmgr.Manager, args []string) int {
	parser := flag.NewFlagSet("worker-add", flag.ExitOnError)
	updateUsage(parser, "<ip> [public-address]")
	parser.Parse(args)

	if parser.NArg() < 1 || parser.NArg() > 2 {
		parser.Usage()
		return USAGE_ERROR
	}

	public := ""
	if parser.NArg() == 2 {
		public = parser.Arg(1)
	}

	if err := mgr.AddWorker(parser.Arg(0), public); err != nil {
		fmt.Printf("error: could not add worker: %s\n", err)
		return RUNTIME_ERROR
	}

	return NO_ERROR
}

func removeWorker(mgr *cmgr.Manager, args []string) int {
	parser := flag.NewFlagSet("worker-remove", flag.ExitOnError)
	updateUsage(parser, "<ip>")
	parser.Parse(args)

	if parser.NArg() != 1 {
		parser.Usage()
		return USAGE_ERROR
	}

	if err := mgr.RemoveWorker(parser.Arg(0)); err != nil {
		fmt.Printf("error: could not remove worker: %s\n", err)
		return RUNTIME_ERROR
	}

	return NO_ERROR
}

func listWorkers(mgr *cmgr.Manager, args []string) int {
	parser := flag.NewFlagSet("worker-list", flag.ExitOnError)
	updateUsage(parser, "")
	parser.Parse(args)

	if parser.NArg() != 0 {
		parser.Usage()
		return USAGE_ERROR
	}

	workers, err := mgr.ListWorkers()
	if err != nil {
		fmt.Printf("error: could not list workers: %s\n", err)
		return RUNTIME_ERROR
	}

	for _, w := range workers {
		public := ""
		if w.Public != "" {
			public = fmt.Sprintf(" (public: %s)", w.Public)
		}
		fmt.Printf("%s%s: %s, %d instance(s)\n", w.IP, public, w.Health, w.Instances)
	}

	return NO_ERROR
}
