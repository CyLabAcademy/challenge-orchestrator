package main

import (
	"strings"
	"testing"
)

// --server is an address, and read as one when the flag is parsed: every
// later use of it is a URL, and checkServer only warns about a server it
// cannot reach, so one no request can be made against would otherwise
// surface as an event built and then refused a challenge at a time. A usage
// error costs nothing; that costs the build.
func TestServerListSet(t *testing.T) {
	for _, ok := range []string{
		"http://cork:4200",
		"https://orchestrator.example.net",
		"http://127.0.0.1:4200",
	} {
		var l serverList
		if err := l.Set(ok); err != nil {
			t.Errorf("--server %q was refused: %s", ok, err)
		}
	}

	// A trailing slash is trimmed rather than refused: the paths this is
	// joined with all start with one.
	var trimmed serverList
	if err := trimmed.Set("http://cork:4200/"); err != nil {
		t.Fatal(err)
	}
	if trimmed[0] != "http://cork:4200" {
		t.Errorf("--server kept its trailing slash: %q", trimmed[0])
	}

	for _, bad := range []struct{ value, says string }{
		{"", "empty"},
		{"http://bad host", "not an address"},   // a space: url.Parse refuses it
		{"cork:4200", "not an http(s) address"}, // scheme "cork"
		{"ftp://cork:4200", "not an http(s) address"},
		{"http://", "names no host"},
		{"/challenges", "not an http(s) address"}, // a path, not an address
	} {
		var l serverList
		err := l.Set(bad.value)
		if err == nil {
			t.Errorf("--server %q was accepted, and no request can be made against it", bad.value)
			continue
		}
		if !strings.Contains(err.Error(), bad.says) {
			t.Errorf("--server %q was refused with %q, which does not say %q", bad.value, err, bad.says)
		}
		if len(l) != 0 {
			t.Errorf("--server %q was refused and collected anyway: %v", bad.value, l)
		}
	}

	// Repeatable, and each one kept: a deployment can have more than one
	// orchestrator, and every one of them wants the same builds.
	var many serverList
	for _, s := range []string{"http://a:4200", "http://b:4200"} {
		if err := many.Set(s); err != nil {
			t.Fatal(err)
		}
	}
	if len(many) != 2 || many.String() != "http://a:4200, http://b:4200" {
		t.Errorf("repeated --server collected as %q (%d)", many.String(), len(many))
	}
}
