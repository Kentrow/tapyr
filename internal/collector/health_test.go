package collector

import (
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestAggregateHealth(t *testing.T) {
	for _, tc := range []struct {
		in   []string
		want string
	}{
		{nil, "unknown"},
		{[]string{"unknown", "unknown"}, "unknown"},
		{[]string{"ok", "unknown"}, "ok"},
		{[]string{"ok", "ok"}, "ok"},
		{[]string{"failing", "failing"}, "failing"},
		{[]string{"ok", "failing"}, "degraded"},
		{[]string{"ok", "degraded"}, "degraded"},
		{[]string{"failing", "degraded"}, "degraded"},
	} {
		if got := aggregateHealth(tc.in...); got != tc.want {
			t.Errorf("aggregateHealth(%v) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

func TestAccountHealth(t *testing.T) {
	ok := func(job string) lastRun { return lastRun{Job: job, Result: "success"} }
	failed := func(job, err string) lastRun { return lastRun{Job: job, Result: "failed", Error: err} }
	for _, tc := range []struct {
		name        string
		in          accountInput
		want        string
		wantReasons []string
	}{
		{name: "no run yet", in: accountInput{}, want: "unknown", wantReasons: []string{}},
		{name: "every run succeeded", in: accountInput{Runs: []lastRun{ok("a"), ok("b")}, Circuits: []circuit{{State: "closed"}}}, want: "ok", wantReasons: []string{}},
		{
			name: "rejected key: every run fails",
			in:   accountInput{Runs: []lastRun{failed("a", "auth /devices: HTTP 401: rejected"), failed("b", "auth /devices: HTTP 401: rejected")}},
			want: "failing", wantReasons: []string{"a, b: auth /devices: HTTP 401: rejected"},
		},
		{
			name: "one job of two fails",
			in:   accountInput{Runs: []lastRun{ok("a"), failed("b", "server /sites: HTTP 502: bad gateway")}},
			want: "degraded", wantReasons: []string{"b: server /sites: HTTP 502: bad gateway"},
		},
		{
			name: "a skipped run degrades and names its result",
			in:   accountInput{Runs: []lastRun{ok("a"), {Job: "b", Result: "skipped_overlap"}}},
			want: "degraded", wantReasons: []string{"b: skipped_overlap"},
		},
		{
			name: "a run skipped by the budget degrades with its error",
			in:   accountInput{Runs: []lastRun{{Job: "a", Result: "skipped_budget", Error: "quota exhausted"}}},
			want: "degraded", wantReasons: []string{"a: quota exhausted"},
		},
		{
			name: "a partial run degrades",
			in:   accountInput{Runs: []lastRun{{Job: "a", Result: "partial", Error: "server /sites/2: HTTP 500"}}},
			want: "degraded", wantReasons: []string{"a: server /sites/2: HTTP 500"},
		},
		{
			name: "re-bootstrap required, runs not failed yet",
			in:   accountInput{Runs: []lastRun{ok("a")}, Reauth: true},
			want: "failing", wantReasons: []string{"the credentials need a manual re-bootstrap"},
		},
		{name: "circuit open", in: accountInput{Circuits: []circuit{{State: "open"}}}, want: "failing", wantReasons: []string{"circuit open"}},
		{name: "circuit half open", in: accountInput{Runs: []lastRun{ok("a")}, Circuits: []circuit{{State: "half_open"}}}, want: "degraded", wantReasons: []string{"circuit half open"}},
		{
			name: "one target of two with its circuit open",
			in:   accountInput{Runs: []lastRun{ok("a")}, Circuits: []circuit{{Target: "north", State: "open"}, {Target: "south", State: "closed"}}},
			want: "degraded", wantReasons: []string{"circuit open for target north"},
		},
		{
			name: "every target with its circuit open",
			in:   accountInput{Circuits: []circuit{{Target: "north", State: "open"}, {Target: "south", State: "open"}}},
			want: "failing", wantReasons: []string{"circuit open for target north", "circuit open for target south"},
		},
		{
			name: "reasons are deduplicated and capped",
			in: accountInput{Runs: []lastRun{
				failed("a", "e1"), failed("a", "e1"), failed("f", "e1"), failed("b", "e2"), failed("c", "e3"), failed("d", "e4"), failed("e", "e5"),
			}},
			want: "failing", wantReasons: []string{"a, f: e1", "b: e2", "c: e3", "and 2 more errors"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, reasons := accountHealth(tc.in)
			if got != tc.want {
				t.Errorf("health = %s, want %s", got, tc.want)
			}
			if diff := cmp.Diff(tc.wantReasons, reasons); diff != "" {
				t.Errorf("reasons (-want +got):\n%s", diff)
			}
		})
	}
}
