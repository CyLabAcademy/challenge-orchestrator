package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/CyLabAcademy/challenge-orchestrator/cork"
)

// serverProbeTimeout bounds the one question asked of an orchestrator before
// anything is sent to it. The hand-over itself gets no timeout (see
// handOver), but this must not hang a build on a box that has gone.
const serverProbeTimeout = 10 * time.Second

// answerLimit bounds what is read of an orchestrator's answer. The answers
// here are one challenge's verdict; a body past this is not one.
const answerLimit = 8 << 20

type serverInfo struct {
	Version    string `json:"version"`
	BuildPlane string `json:"build_plane"`
}

// handOverResponse is the orchestrator's answer to a hand-over, as corkd's
// HandOverResponse writes it. Only what is reported here is decoded.
type handOverResponse struct {
	Added      []string `json:"added"`
	Refreshed  []string `json:"refreshed"`
	Updated    []string `json:"updated"`
	Stale      []string `json:"stale"`
	Unmodified []string `json:"unmodified"`
	Errors     []string `json:"errors"`
	Challenge  struct {
		Builds []struct {
			Id int64 `json:"id"`
		} `json:"builds"`
	} `json:"challenge"`
}

// verdict is the one bucket the challenge came back in, which is what an
// update would have called it.
func (r handOverResponse) verdict() string {
	for _, bucket := range []struct {
		name string
		ids  []string
	}{
		{"added", r.Added}, {"updated", r.Updated}, {"refreshed", r.Refreshed},
		{"stale", r.Stale}, {"unmodified", r.Unmodified},
	} {
		if len(bucket.ids) > 0 {
			return bucket.name
		}
	}
	return "recorded"
}

// checkServer asks an orchestrator what it is before a build is sent to it.
// Two things are worth knowing in advance. One is fatal: a daemon on a local
// build plane builds its own challenges and takes no hand-over, so there is
// nothing to send it. The other is a warning: the orchestrator recomputes
// every build's identity with its own copy of the challenge templates, so
// where its version differs from this one a template that changed between
// them makes every build's identity disagree, and every hand-over is refused
// as invalid. A question that cannot be asked is itself only a warning: the
// hand-over will report what is really wrong.
func checkServer(server string) error {
	client := &http.Client{Timeout: serverProbeTimeout}
	resp, err := client.Get(server + "/version")
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not ask %s what it is: %s\n", server, err)
		return nil
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, answerLimit))
	if err != nil || resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "warning: could not ask %s what it is: %s\n", server, resp.Status)
		return nil
	}
	var info serverInfo
	if err := json.Unmarshal(body, &info); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not read what %s is: %s\n", server, err)
		return nil
	}
	if info.BuildPlane != cork.BuildPlaneExternal {
		return fmt.Errorf("%s builds its own challenges (%s=%s) and takes no hand-over",
			server, cork.BUILD_PLANE_ENV, info.BuildPlane)
	}
	if info.Version != buildVersion() {
		fmt.Fprintf(os.Stderr, "warning: %s runs cork %s and this is %s; if a challenge template changed between them, every build's identity differs there and the hand-over is refused as invalid\n",
			server, info.Version, buildVersion())
	}
	return nil
}

// handOver sends one challenge and its builds to one orchestrator:
// PUT /challenges/<id>, the JSON payload and nothing else. Artifacts do not
// travel with it. They are the build plane's, they stay here, and the
// orchestrator is told only that a build has them (HasArtifacts), which is
// what the platform reads to decide whether to offer a download. What
// serves them reads this plane's artifact directory; see BUILDER.md.
func handOver(server string, id cork.ChallengeId, payload *cork.HandOver) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("handing '%s' over to %s: %w", id, server, err)
	}
	req, err := http.NewRequest(http.MethodPut, server+"/challenges/"+string(id), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("handing '%s' over to %s: %w", id, server, err)
	}
	req.Header.Set("Content-Type", "application/json")

	// No timeout, as the corkd CLI has none for the same reason: a
	// hand-over restarts and relaunches the instances of the generation it
	// replaces, which legitimately takes minutes.
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return fmt.Errorf("handing '%s' over to %s: %w", id, server, err)
	}
	defer resp.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(resp.Body, answerLimit))
	if err != nil {
		return fmt.Errorf("reading what %s answered for '%s': %w", server, id, err)
	}

	var recorded handOverResponse
	decoded := json.Unmarshal(answer, &recorded) == nil
	if resp.StatusCode != http.StatusOK {
		if decoded && len(recorded.Errors) > 0 {
			return fmt.Errorf("handing '%s' over to %s: %s: %s",
				id, server, resp.Status, strings.Join(recorded.Errors, "; "))
		}
		return fmt.Errorf("handing '%s' over to %s: %s: %s",
			id, server, resp.Status, strings.TrimSpace(string(answer)))
	}
	if !decoded {
		return fmt.Errorf("reading what %s answered for '%s': not a hand-over verdict: %s",
			server, id, strings.TrimSpace(string(answer)))
	}
	fmt.Printf("  %s: %s, %d build(s)\n", id, recorded.verdict(), len(recorded.Challenge.Builds))
	return nil
}

// httpClient is what an operation uses: no timeout, as the corkd CLI has
// none, because a release or a converge legitimately takes minutes.
func httpClient() *http.Client { return &http.Client{} }

// readAnswer is as much of a refusal as is worth quoting back.
func readAnswer(resp *http.Response) string {
	body, err := io.ReadAll(io.LimitReader(resp.Body, answerLimit))
	if err != nil {
		return "(its answer could not be read)"
	}
	return strings.TrimSpace(string(body))
}

// convergeOn puts a handed-over schema into service, which is the second
// half of a deploy and the half that was missing.
//
// A hand-over records builds; it does not decide what runs them. The
// instance counts travel with it (openBuild takes them from the payload),
// but only a converge acts on them -- starting what a schema asks for,
// stopping what it no longer does. Until this, a schema handed to an
// orchestrator sat there as rows with nothing serving, and a migration left
// the orchestrator taking it in exactly that state.
//
// POST /schemas/<name>, which is update-schema and not add-schema: the
// hand-over has already made the schema exist as far as this daemon is
// concerned (schemaExists is a query over the builds table, and the rows
// arrived stamped with the name), so a create would be refused for a schema
// that already exists.
func convergeOn(address string, schema *cork.Schema) error {
	body, err := json.Marshal(schema)
	if err != nil {
		return fmt.Errorf("converging schema '%s' on %s: %w", schema.Name, address, err)
	}
	resp, err := httpClient().Post(address+"/schemas/"+schema.Name, "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("converging schema '%s' on %s: %w", schema.Name, address, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("converging schema '%s' on %s: %s: %s", schema.Name, address, resp.Status, readAnswer(resp))
	}
	return nil
}
