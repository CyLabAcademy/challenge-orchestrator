package main

import (
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/CyLabAcademy/challenge-orchestrator/cmgr"
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

// handOverResponse is the orchestrator's answer to a hand-over, as cmgrd's
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
	if info.BuildPlane != cmgr.BuildPlaneExternal {
		return fmt.Errorf("%s builds its own challenges (%s=%s) and takes no hand-over",
			server, cmgr.BUILD_PLANE_ENV, info.BuildPlane)
	}
	if info.Version != buildVersion() {
		fmt.Fprintf(os.Stderr, "warning: %s runs cork %s and this is %s; if a challenge template changed between them, every build's identity differs there and the hand-over is refused as invalid\n",
			server, info.Version, buildVersion())
	}
	return nil
}

// handOver sends one challenge and its builds to one orchestrator:
// PUT /challenges/<id>, multipart, the JSON payload first and then the
// artifact archive of every build that has one, in build order, as the
// endpoint reads them.
func handOver(server string, id cmgr.ChallengeId, payload *cmgr.HandOver, artifactPath func(cmgr.BuildId) string) error {
	body, writer := io.Pipe()
	form := multipart.NewWriter(writer)
	go func() {
		// Written as the request goes out, so a challenge with a gigabyte
		// of bundles is streamed off disk rather than held in memory. An
		// error here closes the pipe with it, which fails the request
		// rather than sending a body the orchestrator would have to guess
		// at.
		writer.CloseWithError(writeHandOver(form, payload, artifactPath))
	}()

	req, err := http.NewRequest(http.MethodPut, server+"/challenges/"+string(id), body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", form.FormDataContentType())

	// No timeout, as the cmgrd CLI has none for the same reason: a
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

// writeHandOver writes the multipart body: the payload as the part
// hand_over, then one part artifacts.<i> for each build at index i that has
// an archive, in build order, which is the order the endpoint reads them in.
func writeHandOver(form *multipart.Writer, payload *cmgr.HandOver, artifactPath func(cmgr.BuildId) string) error {
	part, err := form.CreateFormField("hand_over")
	if err != nil {
		return err
	}
	if err := json.NewEncoder(part).Encode(payload); err != nil {
		return err
	}
	for i, build := range payload.Challenge.Builds {
		if !build.HasArtifacts {
			continue
		}
		path := artifactPath(build.Id)
		archive, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("the archive of build %d of '%s': %w", i, payload.Challenge.Id, err)
		}
		part, err := form.CreateFormFile(fmt.Sprintf("artifacts.%d", i), filepath.Base(path))
		if err != nil {
			archive.Close()
			return err
		}
		_, err = io.Copy(part, archive)
		archive.Close()
		if err != nil {
			return fmt.Errorf("sending the archive of build %d of '%s': %w", i, payload.Challenge.Id, err)
		}
	}
	return form.Close()
}
