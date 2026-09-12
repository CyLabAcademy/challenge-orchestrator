package cmgr

import (
	"context"
	"strings"
	"testing"
)

// The ids the loader really produces are accepted. This half matters more
// than the refusals: a validation that turns away a working challenge breaks
// a deployment that was fine, and every id here is one sanitizeName and the
// namespace rule can actually emit (lowercased, non-alphanumerics turned into
// '-', trimmed, under '/'-separated lowercase alphanumeric segments).
func TestValidChallengeIdAcceptsWhatTheLoaderMakes(t *testing.T) {
	for _, id := range []ChallengeId{
		"binex101",
		"over-the-wire",
		"a",
		"1",
		"crypto/rsa",
		"picoctf/2026/binex101",
		"web/xss-reflected",
		// sanitizeName leaves one '-' per non-alphanumeric, so a name with
		// two spaces or a punctuation run becomes a run of dashes. Refusing
		// these would turn away challenges that build today.
		"a--b",
		"spring-2026--finals",
		"c-c-c",
		"n0t-4-r34l-ch4ll3ng3",
	} {
		if !validChallengeId(id) {
			t.Errorf("'%s' is an id the loader can produce and was refused", id)
		}
	}
}

// And the shapes that are not ids at all. Each of these reaches a registry
// URL path and an image tag, where '#' and '?' end the path and re-aim the
// request and '..' climbs out of /v2/ -- carrying this daemon's registry
// credentials and client certificate.
func TestValidChallengeIdRefusesWhatWouldReAimARequest(t *testing.T) {
	for _, id := range []ChallengeId{
		"",
		"../foo",
		"foo/../../evil",
		"library/binex:s5-1a2b3c4d-web",
		"library/binex#x",
		"library/binex?x",
		"UPPER",
		"has space",
		"trailing-",
		"-leading",
		"double//slash",
		"/leading-slash",
		"trailing-slash/",
		"semi;colon",
		"pct%2fescape",
		"new\nline",
	} {
		if validChallengeId(id) {
			t.Errorf("'%s' was accepted as a challenge id", id)
		}
	}
}

// The refusal reaches the caller as an invalid hand-over (400), before
// anything is written and before the registry is asked about anything.
func TestHandOverRefusesAnIdThatIsNotOne(t *testing.T) {
	f := setupHandOverFixture(t)
	id := ChallengeId("library/binex:s5-1a2b3c4d-web")
	ho := f.handOver(id, 0x1111, 0, delivered(1, "flag{one}", false))

	_, err := f.m.HandOverChallenge(id, ho, UpdateOptions{})
	if err == nil {
		t.Fatal("a challenge id carrying a tag was recorded")
	}
	if !strings.Contains(err.Error(), ErrHandOverInvalid.Error()) {
		t.Errorf("refused with %v, want an invalid hand-over", err)
	}
	if len(*f.seen) != 0 {
		t.Errorf("the registry was asked %d question(s) about a payload that never should have reached it", len(*f.seen))
	}
}

// The second lock, on the one line that turns a name into a request: even
// given a name checkHandOver would have refused, the URL keeps its segments
// rather than letting the name re-divide it. Asserted through the request
// registryManifestRequest builds, since that is where a stray character would
// take effect.
func TestRegistryRequestKeepsItsSegments(t *testing.T) {
	m := &Manager{challengeRegistry: "registry.internal", ctx: context.Background()}
	for _, tc := range []struct {
		image    string
		wantPath string
	}{
		{"registry.internal/binex101:s1-aaaa-web", "/v2/binex101/manifests/s1-aaaa-web"},
		{"registry.internal/crypto/rsa:s1-aaaa-web", "/v2/crypto/rsa/manifests/s1-aaaa-web"},
		// A '#' would otherwise end the path and drop everything after it,
		// turning a HEAD or DELETE on this daemon's own tag into one on
		// whatever the prefix happens to name.
		{"registry.internal/evil#x:s1-aaaa-web", "/v2/evil%23x/manifests/s1-aaaa-web"},
		{"registry.internal/evil?x:s1-aaaa-web", "/v2/evil%3Fx/manifests/s1-aaaa-web"},
	} {
		req, err := m.registryManifestRequest("HEAD", tc.image)
		if err != nil {
			t.Errorf("registryManifestRequest(%s): %s", tc.image, err)
			continue
		}
		if got := req.URL.EscapedPath(); got != tc.wantPath {
			t.Errorf("%s became %s, want %s", tc.image, got, tc.wantPath)
		}
		if req.URL.Host != "registry.internal" {
			t.Errorf("%s was aimed at host %s", tc.image, req.URL.Host)
		}
		if req.URL.Fragment != "" || req.URL.RawQuery != "" {
			t.Errorf("%s left a fragment (%q) or query (%q), so the path was re-divided",
				tc.image, req.URL.Fragment, req.URL.RawQuery)
		}
	}
}

// A build already on record under a different number is the shape of a
// rebuilt build plane. It is refused rather than renumbered, because four
// tables reference builds(id) ON UPDATE RESTRICT and because a row whose id
// no longer matches its bundle is exactly the defect this adoption closes.
func TestHandOverRefusesABuildTheBuildPlaneRenumbered(t *testing.T) {
	f := setupHandOverFixture(t)
	id := ChallengeId("test/renumbered")

	first := delivered(1, "flag{one}", true)
	first.Id = 301
	if _, err := f.m.HandOverChallenge(id, f.handOver(id, 0x1111, 0, first), UpdateOptions{}); err != nil {
		t.Fatalf("first hand-over: %s", err)
	}

	// The same build -- same schema, format, seed and content -- arriving
	// under the number a rebuilt plane drew for it.
	again := delivered(1, "flag{one}", true)
	again.Id = 7
	cu, err := f.m.HandOverChallenge(id, f.handOver(id, 0x1111, 0, again), UpdateOptions{})
	if err != nil {
		t.Fatalf("second hand-over: %s", err)
	}
	if len(cu.Errors) == 0 {
		t.Fatal("a renumbered build was taken silently")
	}
	joined := ""
	for _, e := range cu.Errors {
		joined += e.Error() + "\n"
	}
	if !strings.Contains(joined, "renumbered") {
		t.Errorf("the refusal does not say what happened: %s", joined)
	}
	// And the row still carries the id its bundle is named by.
	if _, err := f.m.GetBuildMetadata(BuildId(301)); err != nil {
		t.Errorf("build 301 is no longer on record: %s", err)
	}
	if _, err := f.m.GetBuildMetadata(BuildId(7)); err == nil {
		t.Error("the renumbered id was recorded after all")
	}
}
