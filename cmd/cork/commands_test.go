package main

import "testing"

// The worker-list line is what an operator reads during an incident, so its
// shape is worth holding still.
func TestLaunchSummary(t *testing.T) {
	cases := []struct {
		name string
		in   LaunchStats
		want string
	}{{
		name: "a worker that has taken launches",
		in:   LaunchStats{Count: 340, Samples: 256, P50Ms: 1200, P90Ms: 4100, MaxMs: 8400},
		want: "  launch=p50 1.2s/p90 4.1s/max 8.4s n=340",
	}, {
		// Sub-second launches read as whole milliseconds: "0.2s" throws away
		// the difference between a fast worker and a very fast one.
		name: "a fast worker",
		in:   LaunchStats{Count: 12, Samples: 12, P50Ms: 180, P90Ms: 240, MaxMs: 950},
		want: "  launch=p50 180ms/p90 240ms/max 950ms n=12",
	}, {
		// Failures are counted but time nothing, so they show beside the
		// quantiles rather than inside them.
		name: "a worker with some failures",
		in:   LaunchStats{Count: 340, Failed: 12, Samples: 256, P50Ms: 1200, P90Ms: 4100, MaxMs: 8400},
		want: "  launch=p50 1.2s/p90 4.1s/max 8.4s n=340 failed=12",
	}, {
		// The case worth getting right: launches are arriving and none is
		// finishing. Three zeroes would read as a very fast worker, and a
		// frozen n would read as a worker nothing is being sent to.
		name: "a worker failing everything",
		in:   LaunchStats{Count: 25, Failed: 25, Samples: 0},
		want: "  launch=none succeeded n=25 failed=25",
	}, {
		// Nothing at all rather than a row of zeroes, which reads like a fast
		// worker rather than an unused one.
		name: "a worker that has taken none",
		in:   LaunchStats{},
		want: "",
	}}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := launchSummary(c.in); got != c.want {
				t.Errorf("launchSummary(%+v) =\n  %q\nwant\n  %q", c.in, got, c.want)
			}
		})
	}
}

func TestShortMillis(t *testing.T) {
	cases := map[int64]string{
		0: "0ms", 1: "1ms", 999: "999ms",
		1000: "1.0s", 1050: "1.1s", 8400: "8.4s", 60000: "60.0s",
	}
	for in, want := range cases {
		if got := shortMillis(in); got != want {
			t.Errorf("shortMillis(%d) = %q, want %q", in, got, want)
		}
	}
}
