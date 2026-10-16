package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/CyLabAcademy/challenge-orchestrator/cork"
)

type PreloadAddRequest struct {
	Schema string `json:"schema"`
}

// preloadsHandler serves the preloaded schemas: GET lists them with each
// worker's progress, POST {"schema": "<name>"} preloads one on every worker.
func (s state) preloadsHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		infos, err := s.mgr.ListPreloads()
		if err != nil {
			preloadError(w, err)
			return
		}
		body, err := json.Marshal(infos)
		if err != nil {
			preloadError(w, err)
			return
		}
		w.Write(body)
	case "POST":
		var req PreloadAddRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Schema == "" {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`invalid request body: want {"schema": "<name>"}`))
			return
		}
		if err := s.mgr.AddPreload(req.Schema); err != nil {
			preloadError(w, err)
			return
		}
		// Said, since with no worker registered there is nothing to preload yet.
		body, _ := json.Marshal(map[string]bool{"workers_configured": s.mgr.WorkersConfigured()})
		w.WriteHeader(http.StatusCreated)
		w.Write(body)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// preloadHandler serves DELETE /preloads/<schema>, which returns the schema to
// pulling on demand, leaves its images where they are, and answers with those
// that leave corkd's cleanup and may remain: {"released": [<image>, ...]}.
func (s state) preloadHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "DELETE" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	schema := strings.TrimPrefix(r.URL.Path, "/preloads/")
	if schema == "" || strings.Contains(schema, "/") {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	released, err := s.mgr.RemovePreload(schema)
	if err != nil {
		preloadError(w, err)
		return
	}
	if released == nil {
		released = []string{}
	}
	body, _ := json.Marshal(map[string][]string{"released": released})
	w.Write(body)
}

func preloadError(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	if _, ok := err.(*cork.UnknownIdentifierError); ok {
		code = http.StatusNotFound
	} else if errors.Is(err, cork.ErrPreloadNeedsRegistry) {
		code = http.StatusConflict
	}
	w.WriteHeader(code)
	w.Write([]byte(err.Error()))
}
