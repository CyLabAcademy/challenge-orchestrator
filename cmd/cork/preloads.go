package main

import (
	"fmt"
	"io"
	"net/url"
	"os"
	"time"
)

// PreloadInfo mirrors cork.PreloadInfo; cork/preload.go is the source of truth.
type PreloadInfo struct {
	Schema  string          `json:"schema"`
	Images  int             `json:"images"`
	Workers []PreloadWorker `json:"workers"`
}

type PreloadWorker struct {
	IP        string           `json:"ip"`
	Reachable string           `json:"reachable"`
	Held      int              `json:"held"`
	Pulling   string           `json:"pulling"`
	Failed    []PreloadFailure `json:"failed"`
	LastPass  *time.Time       `json:"last_pass"`
}

type PreloadFailure struct {
	Image string `json:"image"`
	Error string `json:"error"`
}

func preloadAddCommand(c *client, args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: preload-add <schema>")
		return USAGE_ERROR
	}
	var resp struct {
		WorkersConfigured *bool `json:"workers_configured"`
	}
	if err := c.doJSON("POST", "/preloads", map[string]string{"schema": args[0]}, &resp); err != nil {
		return runtimeError(err)
	}
	if resp.WorkersConfigured != nil && !*resp.WorkersConfigured {
		fmt.Println("Preloaded, but no worker is registered: workers pull the set as they are added.")
		return NO_ERROR
	}
	fmt.Println("Preloading on every worker; preload-list shows how far each has got.")
	return NO_ERROR
}

func preloadRemoveCommand(c *client, args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: preload-remove <schema>")
		return USAGE_ERROR
	}
	var resp struct {
		Released []string `json:"released"`
	}
	if err := c.doJSON("DELETE", "/preloads/"+url.PathEscape(args[0]), nil, &resp); err != nil {
		return runtimeError(err)
	}
	printReleased(os.Stdout, resp.Released)
	return NO_ERROR
}

// printReleased lists what corkd no longer cleans up, as it knows from its
// database: no worker was asked what it holds.
func printReleased(out io.Writer, released []string) {
	if len(released) == 0 {
		fmt.Fprintln(out, "Pulling on demand again; other preloaded schemas still want every image it preloaded.")
		return
	}
	fmt.Fprintf(out, "Pulling on demand again. corkd no longer cleans up these %d image(s), which may remain on the workers, as may older ones of the same repositories, until eviction or an operator removes them:\n", len(released))
	for _, name := range released {
		fmt.Fprintf(out, "  %s\n", name)
	}
}

func preloadListCommand(c *client, args []string) int {
	var infos []PreloadInfo
	if err := c.doJSON("GET", "/preloads", nil, &infos); err != nil {
		return runtimeError(err)
	}
	printPreloads(os.Stdout, infos, time.Now())
	return NO_ERROR
}

func printPreloads(out io.Writer, infos []PreloadInfo, now time.Time) {
	if len(infos) == 0 {
		fmt.Fprintln(out, "no schema is preloaded")
		return
	}
	for _, info := range infos {
		fmt.Fprintf(out, "%s: %d image(s)\n", info.Schema, info.Images)
		for _, w := range info.Workers {
			progress := fmt.Sprintf("%d/%d", w.Held, info.Images)
			state := ""
			switch {
			case w.Pulling != "":
				state = "pulling " + w.Pulling
			case info.Images == 0: // a schema left with no builds
				state = "no images"
			case w.Held == info.Images && w.LastPass != nil:
				state = "ready"
			}
			checked := "not checked yet"
			if w.LastPass != nil {
				checked = fmt.Sprintf("checked %s ago", now.Sub(*w.LastPass).Round(time.Second))
			}
			fmt.Fprintf(out, "  %-15s  %-12s  %9s  %-16s  %s\n", w.IP, w.Reachable, progress, checked, state)
			for _, f := range w.Failed {
				fmt.Fprintf(out, "  %-15s  failed %s: %s\n", "", f.Image, f.Error)
			}
		}
	}
}
