package cmgr

import (
	"context"
	"testing"
)

// TestRegistryManifestRequest covers the distribution API URL for the two
// shapes CORK_REGISTRY takes: a bare host, and a host with a path prefix (a
// namespaced registry), whose API still lives at /v2/ on the host with the
// prefix leading every repository name.
func TestRegistryManifestRequest(t *testing.T) {
	cases := []struct {
		registry, image, want string
	}{
		{"zot.example:5000", "zot.example:5000/cat/chal:s1-abcd-challenge",
			"https://zot.example:5000/v2/cat/chal/manifests/s1-abcd-challenge"},
		{"zot.example:5000/ctf", "zot.example:5000/ctf/cat/chal:s1-abcd-challenge",
			"https://zot.example:5000/v2/ctf/cat/chal/manifests/s1-abcd-challenge"},
		{"zot.example/ctf/events/", "zot.example/ctf/events//chal:tag",
			"https://zot.example/v2/ctf/events/chal/manifests/tag"},
	}
	for _, c := range cases {
		m := &Manager{challengeRegistry: c.registry, ctx: context.Background(), log: newLogger(DISABLED)}
		req, err := m.registryManifestRequest("HEAD", c.image)
		if err != nil {
			t.Errorf("%s: %s", c.registry, err)
			continue
		}
		if got := req.URL.String(); got != c.want {
			t.Errorf("%s: got %s, want %s", c.registry, got, c.want)
		}
		if req.Method != "HEAD" {
			t.Errorf("%s: method %s", c.registry, req.Method)
		}
	}

	m := &Manager{challengeRegistry: "zot.example:5000", ctx: context.Background(), log: newLogger(DISABLED)}
	if _, err := m.registryManifestRequest("HEAD", "other:5000/cat/chal:tag"); err == nil {
		t.Error("expected an error for an image not qualified with the registry")
	}
	if _, err := m.registryManifestRequest("HEAD", "zot.example:5000/cat/chal"); err == nil {
		t.Error("expected an error for an image without a tag")
	}
}
