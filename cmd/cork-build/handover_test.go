package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/CyLabAcademy/challenge-orchestrator/cmgr"
)

// handOverPayload is a challenge as this plane hands one over: two builds,
// the first with an archive and the second without.
func handOverPayload() *cmgr.HandOver {
	first := builtBuild("event", 1, 1)
	first.HasArtifacts = true
	second := builtBuild("event", 2, cmgr.DYNAMIC_INSTANCES)
	second.Instances = nil
	first.Instances = nil
	return &cmgr.HandOver{
		Challenge:      builtChallenge("test/handed-over", 0x1111, first, second),
		PinFingerprint: 0xABCD,
	}
}

// archivesIn writes the archive of every build that has one and returns the
// path function the hand-over reads them back through.
func archivesIn(t *testing.T, payload *cmgr.HandOver) func(cmgr.BuildId) string {
	t.Helper()
	dir := t.TempDir()
	for _, build := range payload.Challenge.Builds {
		if !build.HasArtifacts {
			continue
		}
		content := fmt.Sprintf("archive of build %d", build.Id)
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%d.tar.gz", build.Id)), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return func(id cmgr.BuildId) string {
		return filepath.Join(dir, fmt.Sprintf("%d.tar.gz", id))
	}
}

// takenHandOver is what the orchestrator's endpoint sees.
type takenHandOver struct {
	method   string
	path     string
	payload  cmgr.HandOver
	parts    []string
	archives map[string]string
}

// orchestrator stands in for a cmgrd on an external build plane: it reads a
// hand-over exactly as the endpoint does -- the part hand_over first, then
// the archives in order -- and answers what `answer` says.
func orchestrator(t *testing.T, status int, answer any) (*httptest.Server, *takenHandOver) {
	t.Helper()
	taken := &takenHandOver{archives: map[string]string{}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/version" {
			json.NewEncoder(w).Encode(serverInfo{Version: buildVersion(), BuildPlane: cmgr.BuildPlaneExternal})
			return
		}
		taken.method, taken.path = r.Method, r.URL.Path
		reader, err := r.MultipartReader()
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		for {
			part, err := reader.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				// A hand-over whose archive could not be read closes its
				// body mid-part on purpose; what the parts already taken
				// hold is what the test judges.
				break
			}
			taken.parts = append(taken.parts, part.FormName())
			body, _ := io.ReadAll(part)
			// Recorded, never judged: a hand-over whose archive could not
			// be read aborts its body mid-stream on purpose, and even the
			// part before it can arrive short. What reached here is what
			// the tests judge.
			if part.FormName() == "hand_over" {
				json.Unmarshal(body, &taken.payload)
			} else {
				taken.archives[part.FormName()] = string(body)
			}
			part.Close()
		}
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(answer)
	}))
	t.Cleanup(server.Close)
	return server, taken
}

// A hand-over is the payload and then the archives, in build order, and the
// verdict comes back as the orchestrator recorded it.
func TestHandOver(t *testing.T) {
	payload := handOverPayload()
	server, taken := orchestrator(t, http.StatusOK, map[string]any{
		"added":     []string{"test/handed-over"},
		"challenge": map[string]any{"builds": []map[string]any{{"id": 11}, {"id": 12}}},
	})

	if err := handOver(server.URL, payload.Challenge.Id, payload, archivesIn(t, payload)); err != nil {
		t.Fatalf("handOver: %s", err)
	}
	if taken.method != http.MethodPut || taken.path != "/challenges/test/handed-over" {
		t.Errorf("%s %s", taken.method, taken.path)
	}
	// The endpoint reads hand_over first and each archive by the index of
	// the build it belongs to; the second build has none.
	if strings.Join(taken.parts, ",") != "hand_over,artifacts.0" {
		t.Errorf("parts sent: %v", taken.parts)
	}
	if taken.archives["artifacts.0"] != "archive of build 1" {
		t.Errorf("archive sent: %q", taken.archives["artifacts.0"])
	}
	if taken.payload.PinFingerprint != 0xABCD || taken.payload.Challenge.Id != "test/handed-over" {
		t.Errorf("payload taken: %+v", taken.payload)
	}
	if len(taken.payload.Challenge.Builds) != 2 || taken.payload.Challenge.Builds[0].InstanceCount != 1 {
		t.Errorf("builds taken: %+v", taken.payload.Challenge.Builds)
	}
}

// What the orchestrator refuses is reported as it said it, and an archive
// that is not where it should be fails the hand-over rather than sending a
// body with a hole in it.
func TestHandOverFailures(t *testing.T) {
	payload := handOverPayload()
	refusing, _ := orchestrator(t, http.StatusBadRequest, map[string]any{
		"errors": []string{"invalid hand-over: build 0 carries content checksum aaaa"},
	})
	err := handOver(refusing.URL, payload.Challenge.Id, payload, archivesIn(t, payload))
	if err == nil || !strings.Contains(err.Error(), "content checksum aaaa") {
		t.Fatalf("a refused hand-over: %v", err)
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("the refusal does not carry the status: %s", err)
	}

	accepting, _ := orchestrator(t, http.StatusOK, map[string]any{"added": []string{"test/handed-over"}})
	missing := func(cmgr.BuildId) string { return filepath.Join(t.TempDir(), "gone.tar.gz") }
	if err := handOver(accepting.URL, payload.Challenge.Id, payload, missing); err == nil {
		t.Fatal("a hand-over whose archive is not on disk was sent anyway")
	}
}

// An orchestrator is asked what it is before a build is sent to it: one
// that builds for itself is refused, and a version that differs is a
// warning, since the identities it recomputes may differ with it.
func TestCheckServer(t *testing.T) {
	answer := func(info serverInfo, status int) string {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			json.NewEncoder(w).Encode(info)
		}))
		t.Cleanup(server.Close)
		return server.URL
	}
	external := answer(serverInfo{Version: buildVersion(), BuildPlane: cmgr.BuildPlaneExternal}, http.StatusOK)
	if err := checkServer(external); err != nil {
		t.Errorf("an orchestrator on an external build plane: %v", err)
	}
	local := answer(serverInfo{Version: buildVersion(), BuildPlane: cmgr.BuildPlaneLocal}, http.StatusOK)
	if err := checkServer(local); err == nil || !strings.Contains(err.Error(), "takes no hand-over") {
		t.Errorf("an orchestrator that builds for itself: %v", err)
	}
	// Neither an older orchestrator nor one that cannot be asked stops a
	// hand-over: the hand-over itself reports what is really wrong.
	older := answer(serverInfo{Version: "v0.0.1", BuildPlane: cmgr.BuildPlaneExternal}, http.StatusOK)
	if err := checkServer(older); err != nil {
		t.Errorf("an orchestrator of another version: %v", err)
	}
	if err := checkServer(answer(serverInfo{}, http.StatusInternalServerError)); err != nil {
		t.Errorf("an orchestrator that would not say: %v", err)
	}
}

func TestVerdict(t *testing.T) {
	for _, tc := range []struct {
		response handOverResponse
		want     string
	}{
		{handOverResponse{Added: []string{"a"}}, "added"},
		{handOverResponse{Updated: []string{"a"}}, "updated"},
		{handOverResponse{Unmodified: []string{"a"}}, "unmodified"},
		{handOverResponse{}, "recorded"},
	} {
		if got := tc.response.verdict(); got != tc.want {
			t.Errorf("verdict %q, want %q", got, tc.want)
		}
	}
}
