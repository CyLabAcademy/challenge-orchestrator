package main

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

// preload-list is what an operator watches before launches start: "ready" has
// to mean every image held, on a worker that has actually been checked.
func TestPrintPreloads(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	checked := now.Add(-90 * time.Second)
	infos := []PreloadInfo{{
		Schema: "event",
		Images: 3,
		Workers: []PreloadWorker{
			{IP: "10.0.0.1", Reachable: "ok", Held: 3, LastPass: &checked},
			{IP: "10.0.0.2", Reachable: "ok", Held: 1, Pulling: "r/web:s1", LastPass: &checked,
				Failed: []PreloadFailure{{Image: "r/pwn:s1", Error: "manifest unknown"}}},
			{IP: "10.0.0.3", Reachable: "unresponsive"},
			{IP: "10.0.0.4", Reachable: "ok", Held: 3}, // a count from no pass of its own
		},
	}}

	var out strings.Builder
	printPreloads(&out, infos, now)
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 6 {
		t.Fatalf("got %d lines, want 6:\n%s", len(lines), out.String())
	}
	for i, want := range []string{
		`^event: 3 image\(s\)$`,
		"3/3 .* checked 1m30s ago .* ready$",
		"1/3 .* pulling r/web:s1$",
		"failed r/pwn:s1: manifest unknown$",
		"unresponsive .* 0/3 .* not checked yet",
		`10\.0\.0\.4 .* 3/3 .* not checked yet\s*$`,
	} {
		if !regexp.MustCompile(want).MatchString(lines[i]) {
			t.Errorf("line %d = %q, want it to match %q", i, lines[i], want)
		}
	}

	// A schema left with no builds keeps its preload, and is not "ready".
	out.Reset()
	printPreloads(&out, []PreloadInfo{{Schema: "emptied", Workers: []PreloadWorker{{IP: "10.0.0.1", Reachable: "ok", LastPass: &checked}}}}, now)
	if got := out.String(); strings.Contains(got, "ready") || !strings.Contains(got, "no images") {
		t.Errorf("an emptied schema shows:\n%s", got)
	}

	out.Reset()
	printPreloads(&out, nil, now)
	if got := out.String(); got != "no schema is preloaded\n" {
		t.Errorf("with nothing preloaded: %q", got)
	}
}
