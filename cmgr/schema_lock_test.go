package cmgr

import (
	"testing"
	"time"
)

// TestSchemaOperationsSerializeWithUpdates covers the lock that schema
// operations share with rebuilds: UpdateSchema waits while updateMu is held,
// and UpdateSchema on a schema that does not exist yet — which creates it —
// completes rather than deadlocking on the lock it already holds. No docker
// is needed: a schema with no challenges converges through the database only.
func TestSchemaOperationsSerializeWithUpdates(t *testing.T) {
	mgr := setupTestManager(t)
	defer mgr.db.Close()

	schema := &Schema{Name: "lock-test", FlagFormat: "flag{%s}", Challenges: map[ChallengeId]BuildSpecification{}}

	done := make(chan []error, 1)
	run := func() {
		go func() { done <- mgr.UpdateSchema(schema) }()
	}

	// Held by a rebuild: the schema operation must wait.
	mgr.updateMu.Lock()
	run()
	select {
	case errs := <-done:
		mgr.updateMu.Unlock()
		t.Fatalf("UpdateSchema returned (%v) while updateMu was held by an update", errs)
	case <-time.After(200 * time.Millisecond):
	}
	mgr.updateMu.Unlock()

	// Released: it goes through, taking the create path on the way (a schema
	// with no builds never "exists" by schemaExists, so both runs take it).
	select {
	case errs := <-done:
		if len(errs) > 0 {
			t.Fatalf("UpdateSchema of a missing, empty schema failed: %v", errs)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("UpdateSchema did not complete after updateMu was released: the create path deadlocks on the lock it holds")
	}

	// The lock is released on return: a second operation, and a delete, run.
	run()
	select {
	case errs := <-done:
		if len(errs) > 0 {
			t.Fatalf("second UpdateSchema failed: %v", errs)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second UpdateSchema did not complete: the first did not release updateMu")
	}
	if err := mgr.DeleteSchema(schema.Name); err != nil {
		t.Fatalf("DeleteSchema failed: %s", err)
	}
	if !mgr.updateMu.TryLock() {
		t.Fatal("updateMu still held after the schema operations returned")
	}
	mgr.updateMu.Unlock()
}
