package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/CyLabAcademy/challenge-orchestrator/cork"
)

// cork preload-add and preload-remove tell an operator's typo (404) from a
// corkd that cannot preload at all (409) by the code.
func TestPreloadError(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want int
	}{
		{&cork.UnknownIdentifierError{Type: "preload", Name: "event"}, http.StatusNotFound},
		{cork.ErrPreloadNeedsRegistry, http.StatusConflict},
		{errors.New("database is locked"), http.StatusInternalServerError},
	} {
		rec := httptest.NewRecorder()
		preloadError(rec, tc.err)
		if rec.Code != tc.want {
			t.Errorf("%v answered %d, want %d", tc.err, rec.Code, tc.want)
		}
	}
}
