package main

import (
	"encoding/json"
	"net/http"

	"github.com/CyLabAcademy/challenge-orchestrator/cmgr"
)

// pinsHandler serves the base image pins (see cmgr/basepins.go). GET reports
// the current pins and how many challenge Dockerfiles use each. POST
// re-resolves every base reference the challenge directory names to the digest
// the registry serves right now and persists the result; that is the only
// moment cmgrd consults a mutable tag, since builds use the digests.
//
// A refresh rebuilds nothing, and no later update rebuilds a challenge on its
// account either: change detection reads the challenge directory only. A
// refreshed base reaches a challenge the next time that challenge is rebuilt
// for its own reasons, which makes "refresh, then run the batch update" the
// working order. See BUILDER.md.
func (s state) pinsHandler(w http.ResponseWriter, r *http.Request) {
	var (
		pins []cmgr.BasePin
		err  error
	)
	switch r.Method {
	case "GET":
		pins, err = s.mgr.ListBasePins()
	case "POST":
		pins, err = s.mgr.RefreshBasePins()
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	// A refresh can partially succeed: some pins written, others still on a
	// mutable tag. Report the failure but hand back what was resolved.
	if err != nil && pins == nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(err.Error()))
		return
	}
	body, mErr := json.Marshal(struct {
		Pins  []cmgr.BasePin `json:"pins"`
		Error string         `json:"error,omitempty"`
	}{Pins: pins, Error: errString(err)})
	if mErr != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(mErr.Error()))
		return
	}
	w.Write(body)
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
