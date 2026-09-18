package cork

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// retryManager is a manager with a live context and a captured log, which is
// all retryRegistry reads.
func retryManager(t *testing.T) (*Manager, *bytes.Buffer) {
	t.Helper()
	var logged bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &Manager{log: captureLog(&logged), ctx: ctx}, &logged
}

// An exchange that fails without an answer is tried again, and the answer the
// registry finally gives is the one returned. This is the whole point: a
// hand-over asks the registry once per image and an event is thousands of
// images, so a blip that is not retried refuses a challenge for a connection
// that dropped.
func TestRetryRegistryRetriesUntilItGetsAnAnswer(t *testing.T) {
	m, logged := retryManager(t)
	var calls atomic.Int32
	err := m.retryRegistry("asking", func() error {
		if calls.Add(1) < registryAttempts {
			return errors.New("connection reset by peer")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("retryRegistry: %s", err)
	}
	if got := calls.Load(); got != int32(registryAttempts) {
		t.Errorf("the exchange ran %d time(s), want %d", got, registryAttempts)
	}
	if !bytes.Contains(logged.Bytes(), []byte("connection reset by peer")) {
		t.Errorf("a retried failure was not logged: %s", logged.String())
	}
}

// An exchange that succeeds is run once. Nothing about a retry may cost a
// second round trip on the path every build and every hand-over takes.
func TestRetryRegistryDoesNotRepeatSuccess(t *testing.T) {
	m, _ := retryManager(t)
	var calls atomic.Int32
	if err := m.retryRegistry("asking", func() error {
		calls.Add(1)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("the exchange ran %d times, want 1", got)
	}
}

// Attempts are bounded, and the last error is what comes back. A registry
// that is really down has to fail the operation while an operator is still
// watching, not minutes later.
func TestRetryRegistryGivesUp(t *testing.T) {
	m, _ := retryManager(t)
	var calls atomic.Int32
	want := errors.New("no route to host")
	err := m.retryRegistry("asking", func() error {
		calls.Add(1)
		return want
	})
	if !errors.Is(err, want) {
		t.Errorf("gave up with %v, want the last error", err)
	}
	if got := calls.Load(); got != int32(registryAttempts) {
		t.Errorf("the exchange ran %d time(s), want %d", got, registryAttempts)
	}
}

// A cancelled context stops it: the process is shutting down, and waiting out
// a backoff to ask a question nobody will read is time a stop does not have.
func TestRetryRegistryStopsOnAShutdown(t *testing.T) {
	var logged bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{log: captureLog(&logged), ctx: ctx}
	var calls atomic.Int32
	started := time.Now()
	err := m.retryRegistry("asking", func() error {
		calls.Add(1)
		cancel()
		return errors.New("connection refused")
	})
	if err == nil {
		t.Fatal("a cancelled retry reported success")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("the exchange ran %d times after the context was cancelled, want 1", got)
	}
	if waited := time.Since(started); waited > registryRetryDelay {
		t.Errorf("a cancelled retry waited %s, longer than one backoff", waited)
	}
}

// The status the registry gives is an answer, not a failure, so it is not
// retried -- and 404 is the answer that means "not present", which every
// write-once push and every hand-over check depends on being cheap and
// final. Asserted through registryTagPresent, where a 404 answered twice
// would double the cost of verifying an event.
func TestRegistryTagPresentDoesNotRetryAnAnswer(t *testing.T) {
	var asked atomic.Int32
	host, _ := startFakeRegistry(t, func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		w.WriteHeader(http.StatusNotFound)
	})
	var logged bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	m := &Manager{log: captureLog(&logged), ctx: ctx, challengeRegistry: host}

	present, err := m.registryTagPresent(host + "/test/absent:s1-aaaa-challenge")
	if err != nil {
		t.Fatalf("registryTagPresent: %s", err)
	}
	if present {
		t.Error("a 404 was read as present")
	}
	if got := asked.Load(); got != 1 {
		t.Errorf("the registry was asked %d times for a tag it answered 404 for, want 1", got)
	}
}

// A registry that could not be reached says what to do about it. The
// operator reading this is mid-deploy with a registry that is down, and the
// question they are about to ask is "what do I re-run when it is back" --
// which has one answer for every path here, because an operation that could
// not reach the registry recorded nothing. Asserted where it surfaces: the
// hand-over verification, whose error reaches cork-build's stderr through
// the daemon's response body.
func TestAnUnreachableRegistrySaysWhatToRerun(t *testing.T) {
	f := setupHandOverFixture(t)
	// A registry that accepts the connection and answers nothing, which is
	// what the retry is for and what it finally gives up on.
	f.m.challengeRegistry = unreachableRegistry(t)

	id := ChallengeId("test/registry-down")
	ho := f.handOver(id, 0x1111, 0, delivered(1, "flag{one}", false))
	_, err := f.m.HandOverChallenge(id, ho, UpdateOptions{})
	if err == nil {
		t.Fatal("a hand-over with an unreachable registry was recorded")
	}
	// The guidance itself, not a phrase out of it: what it should say is a
	// question of wording and will be reworded, but that it reaches the
	// operator at all is the contract.
	if !strings.Contains(err.Error(), registryRecovery) {
		t.Errorf("the refusal does not carry the recovery, so an operator is left to work out what to re-run: %s", err)
	}
	// And it is a refusal, not a half-write: the challenge is not on record.
	if _, lookupErr := f.m.lookupChallengeMetadata(id); lookupErr == nil {
		t.Error("the challenge was recorded although the registry could not be asked")
	}
}

// unreachableRegistry is a listener that accepts and answers nothing, so a
// request against it fails without a status rather than with one.
func unreachableRegistry(t *testing.T) string {
	t.Helper()
	host, _ := startFakeRegistry(t, func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("could not hijack: %s", err)
			return
		}
		conn.Close()
	})
	return host
}

// A tag delete that fails is not asked again, where a read that fails is.
// The asymmetry is the point and it is easy to erase by "improving" the
// retry: a failed delete leaks a tag, which costs registry disk and a later
// garbage collection, while retrying it spends the backoff on every image of
// every build of a teardown someone is waiting on. Clearing a schema of
// fifty builds against a registry that is down would spend minutes asking
// again for deletes whose whole contract is that they may fail.
func TestRegistryDeleteTagIsNotRetried(t *testing.T) {
	var asked atomic.Int32
	host, _ := startFakeRegistry(t, func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		// A connection that gives no answer at all, which is what a retry
		// would act on: hijack it and close without writing a response.
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("could not hijack: %s", err)
			return
		}
		conn.Close()
	})
	var logged bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	m := &Manager{log: captureLog(&logged), ctx: ctx, challengeRegistry: host}

	if err := m.registryDeleteTag(host + "/test/gone:s1-aaaa-challenge"); err == nil {
		t.Fatal("a delete the registry never answered reported success")
	}
	if got := asked.Load(); got != 1 {
		t.Errorf("the delete was attempted %d times, want 1: a retired tag is best-effort", got)
	}
}
