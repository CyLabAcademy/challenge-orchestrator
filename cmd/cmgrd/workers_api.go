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

// workerHandler serves one worker: DELETE /workers/<ip> purges it — the
// registry entry and all of its instance records are deleted; containers
// still running on a live worker are left for docker-reaper/manual cleanup.
func (s state) workerHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "DELETE" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	ip := strings.TrimPrefix(r.URL.Path, "/workers/")
	if ip == "" || strings.Contains(ip, "/") {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	if err := s.mgr.RemoveWorker(ip); err != nil {
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
