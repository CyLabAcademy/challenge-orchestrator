package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"sync/atomic"
	"time"
)

// gateState is the admission decision derived from the latest telemetry poll.
type gateState int32

const (
	gateAdmit      gateState = iota // host healthy: allow instance starts
	gateOverloaded                  // telemetry reports overloaded: reject with 503
	gateDown                        // telemetry unreachable: treat host as down, reject with 500
)

func (s gateState) String() string {
	switch s {
	case gateAdmit:
		return "admit"
	case gateOverloaded:
		return "overloaded"
	case gateDown:
		return "down"
	default:
		return "unknown"
	}
}

const (
	telemetryURLEnv = "CMGR_TELEMETRY_URL"

	pollInterval = 500 * time.Millisecond
	pollTimeout  = 250 * time.Millisecond // must stay under pollInterval so a hung agent counts as a miss
	maxMisses    = 2                       // consecutive failed polls before declaring the host down
)

// telemetryGate polls the host-local telemetry agent and exposes a single
// admission decision for instance-start requests. It is updated only by run()
// and read by request handlers, so the decision lives in an atomic.
type telemetryGate struct {
	url    string
	client *http.Client
	state  atomic.Int32 // holds a gateState
}

// deriveTelemetryURL returns CMGR_TELEMETRY_URL verbatim if set (full control of
// host, port, and path); otherwise it defaults to the Docker host (from
// DOCKER_HOST, or 127.0.0.1 for a local socket) on port 2136.
func deriveTelemetryURL() string {
	if v := os.Getenv(telemetryURLEnv); v != "" {
		return v
	}
	host := "127.0.0.1"
	if u, err := url.Parse(os.Getenv("DOCKER_HOST")); err == nil && u.Hostname() != "" {
		host = u.Hostname()
	}
	return fmt.Sprintf("http://%s:2136/health", host)
}

func newTelemetryGate() *telemetryGate {
	g := &telemetryGate{
		url:    deriveTelemetryURL(),
		client: &http.Client{Timeout: pollTimeout},
	}
	// Fail closed: reject starts until the first successful poll proves the
	// host is healthy.
	g.state.Store(int32(gateDown))
	return g
}

// reject reports whether an instance-start must be denied. When deny is true it
// also returns the HTTP status and body to send; deny=false means admit.
func (g *telemetryGate) reject() (code int, msg string, deny bool) {
	switch gateState(g.state.Load()) {
	case gateOverloaded:
		return http.StatusServiceUnavailable, "host overloaded", true
	case gateDown:
		return http.StatusInternalServerError, "telemetry unavailable", true
	default:
		return 0, "", false
	}
}

// run polls the telemetry agent forever; start it in its own goroutine.
func (g *telemetryGate) run() {
	misses := 0
	update := func() {
		overloaded, err := g.poll()
		if err != nil {
			misses++
			if misses >= maxMisses {
				g.set(gateDown) // host is down until telemetry answers again
			}
			// Below the threshold: keep the last decision to ride out a blip.
			return
		}
		misses = 0
		if overloaded {
			g.set(gateOverloaded)
		} else {
			g.set(gateAdmit)
		}
	}

	update() // poll immediately so we don't spend the first interval fail-closed
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for range ticker.C {
		update()
	}
}

// set stores the new decision and logs genuine transitions.
func (g *telemetryGate) set(s gateState) {
	if prev := gateState(g.state.Swap(int32(s))); prev != s {
		log.Printf("telemetry gate: %s -> %s", prev, s)
	}
}

// poll performs one telemetry request and returns the overloaded flag.
func (g *telemetryGate) poll() (bool, error) {
	resp, err := g.client.Get(g.url)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("telemetry returned status %d", resp.StatusCode)
	}
	var body struct {
		Overloaded bool `json:"overloaded"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return false, err
	}
	return body.Overloaded, nil
}
