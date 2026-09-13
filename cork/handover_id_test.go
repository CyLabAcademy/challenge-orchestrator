package cork

import (
	"context"
	"crypto/sha256"
	"fmt"
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

// A seccomp profile travels with the challenge that declares it, and is on
// the record the launch path reads. Without this a challenge whose author
// asked for a narrower syscall set was recorded as having a profile and ran
// under the embedded default: the orchestrator has no challenge directory to
// resolve the text from, and the text is in an unexported field that the
// payload's own JSON cannot carry.
func TestHandOverCarriesASeccompProfile(t *testing.T) {
	const profile = `{"defaultAction":"SCMP_ACT_ERRNO","syscalls":[{"names":["read","write"],"action":"SCMP_ACT_ALLOW"}]}`
	sum := fmt.Sprintf("%x", sha256.Sum256([]byte(profile)))

	f := setupHandOverFixture(t)
	id := ChallengeId("test/seccomp")
	ho := f.handOver(id, 0x1111, 0, delivered(1, "flag{one}", false))
	// As the loader leaves it: the challenge-wide options are the override
	// under the empty host, which is what gets persisted and read back.
	ho.Challenge.ChallengeOptions.Seccomp = &SeccompOptions{Profile: "tight.json", ProfileHash: sum}
	ho.Challenge.ChallengeOptions.Overrides = map[string]ContainerOptions{
		"": {Seccomp: &SeccompOptions{Profile: "tight.json", ProfileHash: sum}},
	}
	ho.SeccompProfiles = map[string]string{"tight.json": profile}

	if _, err := f.m.HandOverChallenge(id, ho, UpdateOptions{}); err != nil {
		t.Fatalf("HandOverChallenge: %s", err)
	}
	recorded, err := f.m.lookupChallengeMetadata(id)
	if err != nil {
		t.Fatalf("lookupChallengeMetadata: %s", err)
	}
	opts := recorded.ChallengeOptions.Seccomp
	if opts == nil {
		t.Fatal("the challenge was recorded with no seccomp options at all")
	}
	// The field startContainers actually reads. A declaration without it is
	// the defect: the launch takes the else-branch and applies the default.
	if opts.effectiveProfile != profile {
		t.Errorf("the profile on record is %q, want the text handed over", opts.effectiveProfile)
	}
	if opts.ProfileHash != sum {
		t.Errorf("the hash on record is %s, want %s", opts.ProfileHash, sum)
	}
}

// A declaration whose text did not arrive is refused, rather than recorded
// and quietly run under a wider policy than its author asked for. Likewise a
// text that does not hash to the declaration, which would otherwise let a
// payload widen a policy by sending a permissive profile under a strict
// one's name.
func TestHandOverRefusesASeccompProfileItCannotStandBehind(t *testing.T) {
	const profile = `{"defaultAction":"SCMP_ACT_ERRNO","syscalls":[{"names":["read"],"action":"SCMP_ACT_ALLOW"}]}`
	sum := fmt.Sprintf("%x", sha256.Sum256([]byte(profile)))

	for _, tc := range []struct {
		name     string
		profiles map[string]string
		want     string
	}{
		{"no text at all", nil, "was not handed over"},
		{"another profile's text", map[string]string{"other.json": profile}, "was not handed over"},
		{"text that is not the declared one", map[string]string{"tight.json": `{"defaultAction":"SCMP_ACT_ALLOW"}`}, "hashes to"},
		{"text that is not a profile", map[string]string{"tight.json": "not json"}, "invalid seccomp profile"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setupHandOverFixture(t)
			id := ChallengeId("test/seccomp-refused")
			ho := f.handOver(id, 0x1111, 0, delivered(1, "flag{one}", false))
			ho.Challenge.ChallengeOptions.Seccomp = &SeccompOptions{Profile: "tight.json", ProfileHash: sum}
			ho.SeccompProfiles = tc.profiles

			_, err := f.m.HandOverChallenge(id, ho, UpdateOptions{})
			if err == nil {
				t.Fatal("a challenge whose declared policy could not be applied was recorded")
			}
			if !strings.Contains(err.Error(), ErrHandOverInvalid.Error()) {
				t.Errorf("refused with %v, want an invalid hand-over", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the refusal does not say %q: %s", tc.want, err)
			}
			if _, lookupErr := f.m.lookupChallengeMetadata(id); lookupErr == nil {
				t.Error("the challenge was recorded despite the refusal")
			}
		})
	}
}

// A challenge that declares no profile is untouched: the embedded policy is
// the documented no-op and must not become a refusal.
func TestHandOverWithNoSeccompProfileIsFine(t *testing.T) {
	f := setupHandOverFixture(t)
	id := ChallengeId("test/no-seccomp")
	ho := f.handOver(id, 0x1111, 0, delivered(1, "flag{one}", false))
	if _, err := f.m.HandOverChallenge(id, ho, UpdateOptions{}); err != nil {
		t.Fatalf("a challenge declaring no seccomp profile was refused: %s", err)
	}
}

// The build plane's half of the seccomp fix, on its own. It pairs with a
// hard refusal on the other side -- a declaration whose text did not arrive
// is refused -- so a collector that silently skips a declaration the applier
// then demands would refuse every deploy of that challenge. Nothing tied the
// two together until this test: every other seccomp test hands
// applySeccompProfiles a map written by hand.
func TestSeccompProfilesCollectsWhatTheApplierDemands(t *testing.T) {
	const tight = `{"defaultAction":"SCMP_ACT_ERRNO","syscalls":[{"names":["read"],"action":"SCMP_ACT_ALLOW"}]}`
	const wide = `{"defaultAction":"SCMP_ACT_ALLOW"}`
	resolved := func(name, text string) *SeccompOptions {
		return &SeccompOptions{
			Profile:          name,
			ProfileHash:      fmt.Sprintf("%x", sha256.Sum256([]byte(text))),
			effectiveProfile: text,
		}
	}

	for _, tc := range []struct {
		name      string
		challenge *ChallengeMetadata
		want      map[string]string
	}{
		{
			name:      "no declaration anywhere",
			challenge: &ChallengeMetadata{Id: "a/plain"},
			want:      nil,
		},
		{
			// Challenge-wide options live in the empty-host override, which
			// is how the loader leaves them and how they are persisted.
			name: "challenge-wide only",
			challenge: &ChallengeMetadata{Id: "a/wide", ChallengeOptions: ChallengeOptions{
				Overrides: map[string]ContainerOptions{"": {Seccomp: resolved("tight.json", tight)}},
			}},
			want: map[string]string{"tight.json": tight},
		},
		{
			name: "one host only",
			challenge: &ChallengeMetadata{Id: "a/host", ChallengeOptions: ChallengeOptions{
				Overrides: map[string]ContainerOptions{"work": {Seccomp: resolved("tight.json", tight)}},
			}},
			want: map[string]string{"tight.json": tight},
		},
		{
			// Two declarations, two filenames: both must travel, or the
			// applier refuses the one that did not.
			name: "challenge-wide and a host that differs",
			challenge: &ChallengeMetadata{Id: "a/both", ChallengeOptions: ChallengeOptions{
				Overrides: map[string]ContainerOptions{
					"":     {Seccomp: resolved("tight.json", tight)},
					"work": {Seccomp: resolved("wide.json", wide)},
				},
			}},
			want: map[string]string{"tight.json": tight, "wide.json": wide},
		},
		{
			// A declaration nothing resolved is not carried: it has no text
			// to carry. The applier turns that into a refusal, which is the
			// intended outcome -- better than shipping a profile that was
			// never read.
			name: "a declaration with no resolved text",
			challenge: &ChallengeMetadata{Id: "a/unresolved", ChallengeOptions: ChallengeOptions{
				Overrides: map[string]ContainerOptions{"": {Seccomp: &SeccompOptions{Profile: "tight.json"}}},
			}},
			want: nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := SeccompProfiles(tc.challenge)
			if len(got) != len(tc.want) {
				t.Fatalf("collected %v, want %v", got, tc.want)
			}
			for name, text := range tc.want {
				if got[name] != text {
					t.Errorf("profile %q collected as %q", name, got[name])
				}
			}
			// The contract that matters: whatever the collector produced,
			// the applier accepts. The two halves are written apart and
			// only this pins them together.
			if err := applySeccompProfiles(tc.challenge, got); err != nil {
				if tc.name != "a declaration with no resolved text" {
					t.Errorf("the applier refused what the collector produced: %s", err)
				}
			} else if tc.name == "a declaration with no resolved text" {
				t.Error("a declaration with no text was accepted, so a profile nobody read would be recorded as applied")
			}
		})
	}
}
