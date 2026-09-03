package main

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/picoCTF/cmgr/cmgr"
)

type WorkerAddRequest struct {
	IP string `json:"ip"`
	// Public is the player-facing address (IP or hostname) surfaced in
	// instance metadata; empty means the private IP doubles as public.
	Public string `json:"public"`
}

// workersHandler serves the worker collection: GET lists all configured
// workers (with public address, health, and instance counts), POST adds one
// or rebuilds an existing one's connection (the recovery path for a worker
// marked down).
func (s state) workersHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		infos, err := s.mgr.ListWorkers()
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(err.Error()))
			return
		}
		body, err := json.Marshal(infos)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(err.Error()))
			return
		}
		w.Write(body)
	case "POST":
		var req WorkerAddRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte("invalid request body: " + err.Error()))
			return
		}
		if err := s.mgr.AddWorker(req.IP, req.Public); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(err.Error()))
			return
		}
		w.WriteHeader(http.StatusCreated)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// WorkerUpdateRequest is the body of PATCH /workers/<ip>. Only "down" may be
// set; the other health states are telemetry-derived.
type WorkerUpdateRequest struct {
	Health string `json:"health"`
}

// workerHandler serves one worker. DELETE /workers/<ip> purges it — the
// registry entry and all of its instance records are deleted; containers
// still running on a live worker are left for docker-reaper/manual cleanup.
// PATCH /workers/<ip> with {"health": "down"} takes it out of placement while
// keeping both, for a box that is about to be terminated.
func (s state) workerHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "DELETE" && r.Method != "PATCH" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	ip := strings.TrimPrefix(r.URL.Path, "/workers/")
	if ip == "" || strings.Contains(ip, "/") {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	var err error
	if r.Method == "PATCH" {
		var req WorkerUpdateRequest
		if decodeErr := json.NewDecoder(r.Body).Decode(&req); decodeErr != nil {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte("invalid request body: " + decodeErr.Error()))
			return
		}
		if req.Health != "down" {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`only "down" may be set manually; recover a worker with POST /workers`))
			return
		}
		err = s.mgr.SetWorkerDown(ip)
	} else {
		err = s.mgr.RemoveWorker(ip)
	}

	if err != nil {
		code := http.StatusInternalServerError
		if _, ok := err.(*cmgr.UnknownIdentifierError); ok {
			code = http.StatusNotFound
		}
		w.WriteHeader(code)
		w.Write([]byte(err.Error()))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
