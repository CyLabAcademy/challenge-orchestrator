package cmgr

import (
	"reflect"
	"testing"
)

// The no-registry row is the one that matters most. BuildKit reads CacheFrom
// entries as registry references, so a bare `challenge:tag` normalizes to
// docker.io/library and turns every build into a Docker Hub lookup -- the
// exposure base pinning exists to remove. Single-host cmgr must offer none.
func TestCacheRefsFor(t *testing.T) {
	const (
		reg   = "registry.example:5000"
		cur   = "registry.example:5000/cat/chal:7-aabbccdd-challenge"
		prior = "registry.example:5000/cat/chal:7-11223344-challenge"
	)

	for _, tc := range []struct {
		name                      string
		host                      string
		registry, image, priorImg string
		want                      []string
	}{
		{
			name:  "no registry offers nothing at all",
			image: "cat/chal:7-aabbccdd-challenge", priorImg: "cat/chal:7-11223344-challenge",
			want: nil,
		},
		{
			name:     "a rebuild offers the generation it displaces first",
			registry: reg, image: cur, priorImg: prior,
			want: []string{prior, cur},
		},
		{
			name:     "a first build has no prior generation",
			registry: reg, image: cur, priorImg: "",
			want: []string{cur},
		},
		{
			name:     "a rebuild that reproduced the same content offers one tag, not two",
			registry: reg, image: cur, priorImg: cur,
			want: []string{cur},
		},
		{
			// The builder host's image is never pushed, so both refs would name
			// tags that cannot exist: two guaranteed 404s per build against the
			// registry, for a cache that could never hit.
			name:     "the host whose image is never pushed offers nothing",
			host:     "builder",
			registry: reg, image: cur, priorImg: prior,
			want: nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := tc.host
			if host == "" {
				host = "challenge"
			}
			got := cacheRefsFor(tc.registry, host, tc.image, tc.priorImg)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("cacheRefsFor() = %v, want %v", got, tc.want)
			}
		})
	}
}
