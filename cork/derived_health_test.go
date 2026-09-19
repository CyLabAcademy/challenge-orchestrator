package cork

import "testing"

// derivedHealth is a contract across three programs: corkd computes it,
// `cork worker-list` prints it, and cork-autoscaler gates an ASG lifecycle
// hook on it. It is deprecated in favour of the two axes and kept only so the
// repositories need not release in lockstep, which makes it exactly the kind
// of thing that gets changed without anyone noticing who reads it.
//
// The whole table is pinned, including the cases nobody would think to break.
func TestDerivedHealthContract(t *testing.T) {
	cases := []struct {
		reachable workerReachable
		load      workerLoad
		want      string
		why       string
	}{
		{workerReachableOk, workerLoadOk, "ok", "the ordinary healthy worker"},
		{workerReachableOk, workerOverloaded, "overloaded", "load is the only axis that can downgrade a reachable worker"},

		// The case that matters most, and the one that is easy to get wrong.
		// An unknown load means the telemetry sidecar is silent; the worker is
		// still reachable and still takes placements, so it reads ok.
		//
		// Consequence worth knowing before changing this: cork-autoscaler's
		// launch hook waits for health == "ok" to decide a newly provisioned
		// box is ready (cork.py Worker.ok, handler.py _wait_ready). Because
		// this derives to ok, a box whose telemetry never came up is admitted
		// in-service rather than being abandoned and rebuilt. That is the
		// right answer for the two static workers, where holding a box out
		// halves the fleet, and the wrong one for an autoscaling group that
		// can replace a box for free. The fix belongs in the autoscaler,
		// which should gate on reachable and load rather than on this string.
		{workerReachableOk, workerLoadUnknown, "ok", "a silent sidecar does not stop a reachable worker taking work"},

		// Reachability wins over load in every combination: a worker that
		// cannot be reached is not overloaded, it is unusable, and saying
		// "overloaded" would tell an operator to look at the wrong thing.
		{workerUnresponsive, workerLoadOk, "unresponsive", ""},
		{workerUnresponsive, workerOverloaded, "unresponsive", "an ejected worker is not described by its last load reading"},
		{workerUnresponsive, workerLoadUnknown, "unresponsive", ""},
		{workerDown, workerLoadOk, "down", ""},
		{workerDown, workerOverloaded, "down", "an operator's assertion outranks a load reading"},
		{workerDown, workerLoadUnknown, "down", ""},
	}

	for _, c := range cases {
		got := derivedHealth(c.reachable, c.load)
		if got != c.want {
			t.Errorf("derivedHealth(%s, %s) = %q, want %q%s",
				c.reachable, c.load, got, c.want, ifWhy(c.why))
		}
	}
}

func ifWhy(why string) string {
	if why == "" {
		return ""
	}
	return " -- " + why
}

// The values themselves are the contract, not just the mapping: a consumer
// switches on these strings, and renaming one is a silent break in a program
// this repository does not build.
func TestDerivedHealthEmitsOnlyTheFourKnownValues(t *testing.T) {
	known := map[string]bool{"ok": true, "overloaded": true, "unresponsive": true, "down": true}
	for _, r := range []workerReachable{workerUnresponsive, workerDown, workerReachableOk} {
		for _, l := range []workerLoad{workerLoadUnknown, workerOverloaded, workerLoadOk} {
			if got := derivedHealth(r, l); !known[got] {
				t.Errorf("derivedHealth(%s, %s) = %q, which no consumer knows how to read", r, l, got)
			}
		}
	}
}

// A consumer that only asks `health == "ok"` -- which is what cork-autoscaler
// does -- must never see ok for a worker that cannot take a placement. This is
// the one property the deprecated field has to keep for as long as it exists.
func TestDerivedHealthOkImpliesPlaceable(t *testing.T) {
	for _, r := range []workerReachable{workerUnresponsive, workerDown, workerReachableOk} {
		for _, l := range []workerLoad{workerLoadUnknown, workerOverloaded, workerLoadOk} {
			w := &workerConn{}
			w.reachable.Store(int32(r))
			w.load.Store(int32(l))
			if derivedHealth(r, l) == "ok" && !placeable(w) {
				t.Errorf("derivedHealth(%s, %s) is ok but the worker is not placeable: a consumer reading the deprecated field would place on it", r, l)
			}
		}
	}
}
