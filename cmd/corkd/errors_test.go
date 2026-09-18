package main

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/CyLabAcademy/challenge-orchestrator/cork"
)

// The response code is a contract the platform reads: a 503 tells a caller to
// place the launch again, a 500 tells it the launch failed. The three worker
// exhaustion errors straddle that line and are one identifier-word apart, so
// each is pinned here by name. Unresponsive is the one that matters most and
// the one most recently added: a fleet that is rebooting answers it for as
// long as the reboot takes, and answering 500 there would have the platform
// report a launch failed seconds before the box came back.
func TestErrorResponseLaunch(t *testing.T) {
	tests := []struct {
		name  string
		err   error
		code  int
		retry bool
	}{
		{"all workers overloaded is retryable", cork.ErrAllWorkersOverloaded, http.StatusServiceUnavailable, true},
		{"all workers unresponsive is retryable", cork.ErrAllWorkersUnresponsive, http.StatusServiceUnavailable, true},
		{"all workers down is not", cork.ErrAllWorkersDown, http.StatusInternalServerError, false},
		{"worker busy is retryable", cork.ErrWorkerBusy, http.StatusServiceUnavailable, true},
		{"one worker down is retryable", cork.ErrWorkerUnreachable, http.StatusServiceUnavailable, true},
		{"pull timeout is retryable", cork.ErrPullTimeout, http.StatusServiceUnavailable, true},
		{"database busy is retryable", cork.ErrDatabaseBusy, http.StatusServiceUnavailable, true},
		{"unknown identifier is a 404", &cork.UnknownIdentifierError{Type: "build", Name: "7"}, http.StatusNotFound, false},
		{"anything else is a 500", errors.New("the daemon said no"), http.StatusInternalServerError, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, retry := errorResponse(tc.err, retryableLaunch)
			if code != tc.code || retry != tc.retry {
				t.Fatalf("got (%d, retry=%t), want (%d, retry=%t)", code, retry, tc.code, tc.retry)
			}
		})
	}
}

// A stop places nothing, so only the database's write lock is retryable there.
// The launch-only errors must not pick up a 503 on this path by accident.
func TestErrorResponseStop(t *testing.T) {
	if code, retry := errorResponse(cork.ErrDatabaseBusy, retryableStop); code != http.StatusServiceUnavailable || !retry {
		t.Fatalf("a busy database on a stop: got (%d, retry=%t), want (503, retry=true)", code, retry)
	}

	for _, err := range []error{
		cork.ErrAllWorkersOverloaded,
		cork.ErrWorkerBusy,
		cork.ErrWorkerUnreachable,
		cork.ErrPullTimeout,
	} {
		if code, retry := errorResponse(err, retryableStop); code != http.StatusInternalServerError || retry {
			t.Fatalf("%v on a stop: got (%d, retry=%t), want (500, retry=false)", err, code, retry)
		}
	}
}

// The retryable sentinels reach the handler wrapped by the launch path, so the
// match has to survive the wrapping that adds the worker and instance.
func TestErrorResponseSeesThroughWrapping(t *testing.T) {
	err := fmt.Errorf("%w: worker %s, during the launch of instance %d: %v",
		cork.ErrWorkerUnreachable, "10.0.0.1", 42, errors.New("connection refused"))

	code, retry := errorResponse(err, retryableLaunch)
	if code != http.StatusServiceUnavailable || !retry {
		t.Fatalf("got (%d, retry=%t), want (503, retry=true)", code, retry)
	}
}

// Documents a real limit rather than an intention: the unknown-identifier test
// is a type assertion, not errors.As, so a wrapped one reports 500 instead of
// 404. The manager returns these bare today, which is why the handlers have
// always got away with it. If one ever starts arriving wrapped, this test
// fails and says what to change.
func TestErrorResponseMissesAWrappedUnknownIdentifier(t *testing.T) {
	bare := &cork.UnknownIdentifierError{Type: "instance", Name: "9"}
	if code, _ := errorResponse(bare, retryableStop); code != http.StatusNotFound {
		t.Fatalf("bare unknown identifier: got %d, want 404", code)
	}

	wrapped := fmt.Errorf("looking up the instance: %w", bare)
	if code, _ := errorResponse(wrapped, retryableStop); code != http.StatusInternalServerError {
		t.Fatalf("wrapped unknown identifier: got %d, want 500 (errors.As would make this a 404)", code)
	}
}
