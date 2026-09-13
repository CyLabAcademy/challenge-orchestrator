package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/CyLabAcademy/challenge-orchestrator/cork"
)

// handOverPayload is a challenge as this plane hands one over: two builds,
// the first having published artifacts and the second not. Neither carries
// them -- HasArtifacts is a fact about the build, not a promise of bytes.
func handOverPayload() *cork.HandOver {
	first := builtBuild("event", 1, 1)
	first.HasArtifacts = true
	second := builtBuild("event", 2, cork.DYNAMIC_INSTANCES)
	second.Instances = nil
	first.Instances = nil
	return &cork.HandOver{
		Challenge:      builtChallenge("test/handed-over", 0x1111, first, second),
		PinFingerprint: 0xABCD,
	}
}

// takenHandOver is what the orchestrator's endpoint sees.
type takenHandOver struct {
	method      string
	path        string
	contentType string
	payload     cork.HandOver
	body        []byte
}

// orchestrator stands in for a corkd on an external build plane: it reads a
// hand-over exactly as the endpoint does -- one JSON body -- and answers
// what `answer` says.
func orchestrator(t *testing.T, status int, answer any) (*httptest.Server, *takenHandOver) {
	t.Helper()
	taken := &takenHandOver{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/version" {
			json.NewEncoder(w).Encode(serverInfo{Version: buildVersion(), BuildPlane: cork.BuildPlaneExternal})
			return
		}
		taken.method, taken.path = r.Method, r.URL.Path
		taken.contentType = r.Header.Get("Content-Type")
		taken.body, _ = io.ReadAll(r.Body)
		json.Unmarshal(taken.body, &taken.payload)
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(answer)
	}))
	t.Cleanup(server.Close)
	return server, taken
}

// A hand-over is one JSON payload and nothing else, and the verdict comes
// back as the orchestrator recorded it.
func TestHandOver(t *testing.T) {
	payload := handOverPayload()
	server, taken := orchestrator(t, http.StatusOK, map[string]any{
		"added":     []string{"test/handed-over"},
		"challenge": map[string]any{"builds": []map[string]any{{"id": 11}, {"id": 12}}},
	})

	if err := handOver(server.URL, payload.Challenge.Id, payload); err != nil {
		t.Fatalf("handOver: %s", err)
	}
	if taken.method != http.MethodPut || taken.path != "/challenges/test/handed-over" {
		t.Errorf("%s %s", taken.method, taken.path)
	}
	if taken.contentType != "application/json" {
		t.Errorf("sent as %q, want application/json", taken.contentType)
	}
	// Nothing but the payload travels. A body that had grown a multipart
	// wrapper back, or an archive appended to it, would say so here.
	if !json.Valid(taken.body) {
		t.Errorf("the body is not JSON: %q", firstBytes(taken.body))
	}
	if taken.payload.PinFingerprint != 0xABCD || taken.payload.Challenge.Id != "test/handed-over" {
		t.Errorf("payload taken: %+v", taken.payload)
	}
	if len(taken.payload.Challenge.Builds) != 2 || taken.payload.Challenge.Builds[0].InstanceCount != 1 {
		t.Errorf("builds taken: %+v", taken.payload.Challenge.Builds)
	}
	// The one thing said about artifacts: that the build published them.
	if !taken.payload.Challenge.Builds[0].HasArtifacts || taken.payload.Challenge.Builds[1].HasArtifacts {
		t.Errorf("has_artifacts taken: %v, %v",
			taken.payload.Challenge.Builds[0].HasArtifacts, taken.payload.Challenge.Builds[1].HasArtifacts)
	}
}

// firstBytes is as much of a body as is worth quoting back in a failure.
func firstBytes(body []byte) string {
	if len(body) > 200 {
		return string(body[:200]) + "..."
	}
	return string(body)
}

// What the orchestrator refuses is reported as it said it, and a server no
// request can be made against fails saying what was wrong with it.
func TestHandOverFailures(t *testing.T) {
	payload := handOverPayload()
	refusing, _ := orchestrator(t, http.StatusBadRequest, map[string]any{
		"errors": []string{"invalid hand-over: build 0 carries content checksum aaaa"},
	})
	err := handOver(refusing.URL, payload.Challenge.Id, payload)
	if err == nil || !strings.Contains(err.Error(), "content checksum aaaa") {
		t.Fatalf("a refused hand-over: %v", err)
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("the refusal does not carry the status: %s", err)
	}

	err = handOver("http://bad host", payload.Challenge.Id, payload)
	if err == nil {
		t.Fatal("a hand-over to a server no request can be made against was sent")
	}
	if !strings.Contains(err.Error(), "bad host") {
		t.Errorf("the error does not say what was wrong with the server: %s", err)
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
	external := answer(serverInfo{Version: buildVersion(), BuildPlane: cork.BuildPlaneExternal}, http.StatusOK)
	if err := checkServer(external); err != nil {
		t.Errorf("an orchestrator on an external build plane: %v", err)
	}
	local := answer(serverInfo{Version: buildVersion(), BuildPlane: cork.BuildPlaneLocal}, http.StatusOK)
	if err := checkServer(local); err == nil || !strings.Contains(err.Error(), "takes no hand-over") {
		t.Errorf("an orchestrator that builds for itself: %v", err)
	}
	// Neither an older orchestrator nor one that cannot be asked stops a
	// hand-over: the hand-over itself reports what is really wrong.
	older := answer(serverInfo{Version: "v0.0.1", BuildPlane: cork.BuildPlaneExternal}, http.StatusOK)
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
