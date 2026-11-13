package collector

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
)

// Health values of the status API: ok, degraded, failing, or unknown
// before any run.
const (
	healthUnknown  = "unknown"
	healthOK       = "ok"
	healthDegraded = "degraded"
	healthFailing  = "failing"
)

// maxHealthReasons caps the reasons of an account: the first ones say what
// to look at, the logs and the job pages say the rest.
const maxHealthReasons = 3

// aggregateHealth combines the health of parts of a whole (the jobs of an
// account, the accounts of a provider): unknown when no part is known, ok
// or failing when every known part is, degraded otherwise.
func aggregateHealth(parts ...string) string {
	out := healthUnknown
	for _, h := range parts {
		switch {
		case h == healthUnknown || h == "":
		case out == healthUnknown:
			out = h
		case out != h:
			return healthDegraded
		}
	}
	return out
}

// lastRun is the last run of one job instance of an account.
type lastRun struct {
	Job    string
	Result string
	// Error is the redacted summary of the run, empty on success.
	Error string
}

// circuit is the state of a circuit breaker of an account: the account
// itself (Target empty) or one of its targets.
type circuit struct {
	Target string
	State  string // closed, half_open or open
}

// accountInput is what the health of an account is made of.
type accountInput struct {
	Runs     []lastRun
	Reauth   bool
	Circuits []circuit
}

// runHealth is the health of one run: ok when it succeeded, failing when it
// failed, degraded when it collected part of the data or was skipped (by
// the budget, an overlap or an open circuit: nothing failed, nothing was
// collected either).
func runHealth(result string) string {
	switch result {
	case "success":
		return healthOK
	case "failed":
		return healthFailing
	case "":
		return healthUnknown
	}
	return healthDegraded
}

// accountHealth returns the health of an account and the reasons when it
// is not ok: its jobs (the aggregate of their last runs), its credentials
// (a re-bootstrap required fails it) and its circuits (open for the
// account or every target fails it, open for some targets or half open
// degrades it). The jobs that share an error share a reason.
func accountHealth(in accountInput) (string, []string) {
	reasons := []string{}
	var runs []string
	jobs := map[string][]string{}
	for _, r := range in.Runs {
		h := runHealth(r.Result)
		runs = append(runs, h)
		if h == healthOK || h == healthUnknown {
			continue
		}
		why := cmp.Or(r.Error, r.Result)
		if !slices.Contains(jobs[why], r.Job) {
			jobs[why] = append(jobs[why], r.Job)
		}
	}
	health := aggregateHealth(runs...)
	var failures []string
	for why, names := range jobs {
		slices.Sort(names)
		failures = append(failures, strings.Join(names, ", ")+": "+why)
	}
	slices.Sort(failures)
	if len(failures) > maxHealthReasons {
		more := len(failures) - maxHealthReasons
		failures = append(failures[:maxHealthReasons], fmt.Sprintf("and %d more errors", more))
	}
	reasons = append(reasons, failures...)

	if in.Reauth {
		health = worseHealth(health, healthFailing)
		reasons = append(reasons, "the credentials need a manual re-bootstrap")
	}

	var open, halfOpen []string
	for _, c := range in.Circuits {
		switch c.State {
		case "open":
			open = append(open, c.Target)
		case "half_open":
			halfOpen = append(halfOpen, c.Target)
		}
	}
	switch {
	case len(open) > 0 && len(open) == len(in.Circuits):
		health = worseHealth(health, healthFailing)
	case len(open)+len(halfOpen) > 0:
		health = worseHealth(health, healthDegraded)
	}
	reasons = append(reasons, circuitReasons("circuit open", open)...)
	reasons = append(reasons, circuitReasons("circuit half open", halfOpen)...)
	return health, reasons
}

// circuitReasons names the circuits in a state: the account's, or those of
// its targets.
func circuitReasons(state string, targets []string) []string {
	var out []string
	for _, t := range targets {
		if t == "" {
			out = append(out, state)
			continue
		}
		out = append(out, state+" for target "+t)
	}
	return out
}

// worseHealth returns the worse of two health values.
func worseHealth(a, b string) string {
	rank := map[string]int{"": 0, healthUnknown: 0, healthOK: 1, healthDegraded: 2, healthFailing: 3}
	if rank[b] > rank[a] {
		return b
	}
	return a
}
