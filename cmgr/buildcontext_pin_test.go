package cmgr

import (
	"archive/tar"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The one thing the whole pinning feature exists to do: put a digest-pinned
// FROM into the tar the daemon builds from, and leave the file on disk alone.
//
// Nothing asserted that until this test. pinDockerfile was covered exhaustively
// as a pure function, but making pinBases a no-op -- disabling the feature
// entirely -- left the unit suite AND the e2e green: the e2e's checks (the map
// is populated, the rebuild succeeds, the checksum moved, the tag was pushed,
// the on-disk Dockerfile is untouched) are every one of them satisfied by a
// build that was never rewritten. "Untouched on disk" is trivially true when
// nothing rewrites anything.
//
// No docker: createBuildContext needs only a challenge path, a logger and the
// pin map.
func TestBuildContextCarriesThePinnedDockerfile(t *testing.T) {
	root := t.TempDir()
	chalDir := filepath.Join(root, "chal")
	if err := os.MkdirAll(filepath.Join(chalDir, "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The challenge's own Dockerfile, and a nested one that must NOT be pinned:
	// only the challenge root's is what docker builds.
	write := func(rel, body string) {
		if err := os.WriteFile(filepath.Join(chalDir, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("Dockerfile", "FROM ubuntu:24.04 AS base\nRUN true\n")
	write(filepath.Join("app", "Dockerfile"), "FROM ubuntu:24.04\nRUN true\n")
	write("problem.md", "# chal\n")

	m := &Manager{log: newLogger(DISABLED)}
	m.basePins = &basePins{path: filepath.Join(root, "pins.json"), m: testPins()}

	members := func(t *testing.T, ctxFile string) map[string]string {
		t.Helper()
		f, err := os.Open(ctxFile)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		out := map[string]string{}
		tr := tar.NewReader(f)
		for {
			hdr, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(tr)
			if err != nil {
				t.Fatal(err)
			}
			// The header size is what the daemon reads. If it were taken from
			// the file on disk rather than the rewritten bytes, the entry would
			// be truncated or the write would fail -- so comparing it to the
			// body length is not redundant with the content check below.
			if hdr.Size != int64(len(body)) {
				t.Errorf("%s: header size %d but %d bytes of content", hdr.Name, hdr.Size, len(body))
			}
			out[hdr.Name] = string(body)
		}
		return out
	}

	t.Run("a challenge's own Dockerfile", func(t *testing.T) {
		cm := &ChallengeMetadata{Path: filepath.Join(chalDir, "problem.md")}
		ctxFile, err := m.createBuildContext(cm, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer os.Remove(ctxFile)
		got := members(t, ctxFile)

		if !strings.Contains(got["Dockerfile"], "FROM ubuntu@"+ubuntuDigest) {
			t.Errorf("the Dockerfile the daemon builds from was not pinned:\n%s", got["Dockerfile"])
		}
		if strings.Contains(got["app/Dockerfile"], "@sha256:") {
			t.Errorf("a nested Dockerfile was pinned; only the challenge root's is built:\n%s", got["app/Dockerfile"])
		}
		// ...and the file on disk is still readable by its author.
		onDisk, err := os.ReadFile(filepath.Join(chalDir, "Dockerfile"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(onDisk), "FROM ubuntu:24.04 AS base") {
			t.Errorf("the Dockerfile on disk was rewritten: %s", onDisk)
		}
	})

	t.Run("a built-in template", func(t *testing.T) {
		// A built-in challenge type has NO Dockerfile of its own -- the
		// template is the only one. The fixture must reflect that: with a
		// Dockerfile on disk as well, the walk writes a second tar member
		// under the same name and reading "the" Dockerfile back finds the
		// wrong one, which hid a mutation that skipped pinning the template.
		builtinDir := filepath.Join(root, "builtin")
		if err := os.MkdirAll(builtinDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(builtinDir, "problem.md"), []byte("# chal\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		cm := &ChallengeMetadata{Path: filepath.Join(builtinDir, "problem.md")}
		ctxFile, err := m.createBuildContext(cm, []byte("FROM ubuntu:24.04 AS base\nRUN true\n"))
		if err != nil {
			t.Fatal(err)
		}
		defer os.Remove(ctxFile)
		if got := members(t, ctxFile)["Dockerfile"]; !strings.Contains(got, "FROM ubuntu@"+ubuntuDigest) {
			t.Errorf("the built-in template was not pinned on its way into the context:\n%s", got)
		}
	})
}
