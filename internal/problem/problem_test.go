package problem

import (
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/kentrow/tapyr/internal/budget/planner"
	"github.com/kentrow/tapyr/internal/config"
)

func TestFor(t *testing.T) {
	refs := map[string]config.SecretRef{"apiKey": {Env: "TAPYR_LAB_API_KEY"}}
	until := time.Date(2026, time.November, 16, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name                       string
		in                         Input
		class, title, action, docs string
	}{
		{
			name: "rejected key", in: Input{Class: "auth", Status: 401, Subject: "/sites", Account: "lab", Secrets: refs},
			class: ClassAuth, title: "The provider rejected the credentials of lab.",
			action: "Check the secret TAPYR_LAB_API_KEY (environment variable), then reload Tapyr.", docs: anchorAuth,
		},
		{
			name: "rejected token request", in: Input{Class: "auth", Account: "lab", Secrets: map[string]config.SecretRef{
				"clientSecret": {File: "secrets/secret"}, "clientId": {Env: "LAB_ID"},
			}},
			class: ClassAuth, title: "The provider rejected the credentials of lab.",
			action: "Check the secrets LAB_ID (environment variable) and secrets/secret (file), then reload Tapyr.", docs: anchorAuth,
		},
		{
			name: "missing permission", in: Input{Class: "auth", Status: 403, Subject: "/sites", Account: "lab", Secrets: refs},
			class: ClassForbidden, title: "The credentials of lab lack a permission for /sites.", docs: anchorAuth,
		},
		{
			name: "re-bootstrap", in: Input{Class: ClassReauth, Account: "lab", Secrets: refs},
			class: ClassReauth, title: "The credentials of lab can no longer be renewed.",
			action: "Create new ones as the provider README explains, update TAPYR_LAB_API_KEY (environment variable), then reload Tapyr.", docs: anchorAuth,
		},
		{
			name: "rate limited", in: Input{Class: "rate_limited", Status: 429, Account: "lab"},
			class: ClassRateLimited, title: "The provider is rate limiting the requests of lab (HTTP 429).", docs: anchorQuota,
		},
		{
			name: "quota", in: Input{Class: "quota_exhausted", Subject: "example-sim/daily", Until: until, Account: "lab"},
			class: ClassQuotaExhausted, title: "The daily quota of lab is used up.", docs: anchorQuota,
		},
		{name: "penalized", in: Input{Class: "penalized", Account: "lab"}, class: ClassPenalized, docs: anchorQuota},
		{name: "deadline", in: Input{Class: "deadline"}, class: ClassDeadline, docs: anchorSlowJobs},
		{name: "circuit", in: Input{Class: "circuit_open", Account: "lab"}, class: ClassCircuitOpen, docs: anchorProvider},
		{
			name: "server error", in: Input{Class: "transient", Status: 503}, class: ClassServerError,
			title: "The provider failed to answer (HTTP 503).", docs: anchorProvider,
		},
		{
			name: "timeout", in: Input{Class: "transient"}, class: ClassUnreachable,
			title: "The provider did not answer (no response, or too late).", docs: anchorProvider,
		},
		{
			name: "not found", in: Input{Class: "client", Status: 404, Subject: "/networks/{networkId}/devices"}, class: ClassClient,
			title: "The provider found nothing at /networks/{networkId}/devices (HTTP 404).", docs: anchorMetrics,
		},
		{
			name: "bad request", in: Input{Class: "client", Status: 400, Subject: "/sites"}, class: ClassClient,
			title: "The provider refused the request for /sites (HTTP 400).", docs: anchorMetrics,
		},
		{name: "decode", in: Input{Class: "decode", Status: 200, Subject: "/sites"}, class: ClassDecode, docs: anchorMetrics},
		{name: "mapping", in: Input{Class: ClassMapping}, class: ClassMapping, docs: anchorMetrics},
		{name: "unknown", in: Input{}, class: ClassUnknown, title: "The run failed.", docs: anchorHelp},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := For(tc.in)
			if p.Class != tc.class || tc.title != "" && p.Title != tc.title || tc.action != "" && p.Action != tc.action ||
				!strings.HasSuffix(p.DocsURL, "#"+tc.docs) {
				t.Errorf("For(%+v) = %+v", tc.in, p)
			}
			if p.Title == "" || p.Action == "" || !strings.HasSuffix(p.Title, ".") || !strings.HasSuffix(p.Action, ".") {
				t.Errorf("a problem has a sentence and an action: %+v", p)
			}
			if (p.Until != nil) != !tc.in.Until.IsZero() {
				t.Errorf("until = %v, want %v", p.Until, tc.in.Until)
			}
		})
	}
}

func TestDocsURL(t *testing.T) {
	for version, want := range map[string]string{
		"0.5.0":                  "https://docs.tapyr.eu/0.5/troubleshooting/#quota-problems",
		"v0.5.2":                 "https://docs.tapyr.eu/0.5/troubleshooting/#quota-problems",
		"0.5.0-rc.1":             "https://docs.tapyr.eu/dev/troubleshooting/#quota-problems",
		"0.0.0-dev":              "https://docs.tapyr.eu/dev/troubleshooting/#quota-problems",
		"0.4.0-7-gbfd4bf9-dirty": "https://docs.tapyr.eu/dev/troubleshooting/#quota-problems",
	} {
		if got := DocsURL(version, anchorQuota); got != want {
			t.Errorf("DocsURL(%q) = %s, want %s", version, got, want)
		}
	}
}

// TestAnchorsExist checks that every section a problem links to is a
// heading of the troubleshooting guide, with the anchor GitHub and the
// documentation site give it.
func TestAnchorsExist(t *testing.T) {
	data, err := os.ReadFile("../../docs/troubleshooting.md")
	if err != nil {
		t.Fatal(err)
	}
	anchors := map[string]bool{}
	strip := regexp.MustCompile(`[^a-z0-9 -]`)
	for line := range strings.SplitSeq(string(data), "\n") {
		if title, ok := strings.CutPrefix(strings.TrimLeft(line, "#"), " "); ok && strings.HasPrefix(line, "#") {
			anchors[strings.ReplaceAll(strip.ReplaceAllString(strings.ToLower(title), ""), " ", "-")] = true
		}
	}
	for _, a := range []string{anchorAuth, anchorQuota, anchorSlowJobs, anchorProvider, anchorMetrics, anchorHelp} {
		if !anchors[a] {
			t.Errorf("no section #%s in docs/troubleshooting.md", a)
		}
	}
}

func TestForPlan(t *testing.T) {
	scope := planner.ScopePlan{Limit: "example-sim/daily", Key: "account=lab", Feasible: false}
	day := 24 * time.Hour
	for _, tc := range []struct {
		name          string
		shortfall     planner.Shortfall
		title, action string
	}{
		{
			name: "one critical job",
			shortfall: planner.Shortfall{
				Needed: 4679.4, Budget: 4500, Period: day, MaxStretch: 12, Factor: 1.43, FirstFactor: 1.43,
				Critical: []planner.CriticalJob{{Name: "devices", Desired: 2 * time.Minute, Calls: 4320}},
			},
			title:  "The daily quota of account lab is too small: its jobs need at least 4,679 calls a day, it leaves 4,500.",
			action: "Raise the interval of devices from 2m to 3m, or lower its priority to high so that Tapyr can slow it down.",
		},
		{
			name: "several critical jobs, online",
			shortfall: planner.Shortfall{
				Needed: 1234567, Budget: 1000000, Period: 8*time.Hour + 30*time.Minute + 20*time.Second, Online: true, MaxStretch: 12, Factor: 1.25,
				Critical: []planner.CriticalJob{
					{Name: "a", Desired: 30 * time.Second}, {Name: "b", Desired: time.Hour}, {Name: "c", Desired: 5 * time.Minute}, {Name: "d", Desired: time.Minute},
				},
			},
			title:  "The daily quota of account lab is too small: its jobs need at least 1,234,567 calls over the next 8h30m, it leaves 1,000,000.",
			action: "Raise the intervals of the critical jobs about x1.3 (a from 30s to 38s, b from 1h to 1h15m, c from 5m to 7m, and 1 more), or lower their priority to high so that Tapyr can slow them down.",
		},
		{
			name: "one critical job makes room on its own",
			shortfall: planner.Shortfall{
				Needed: 29551, Budget: 4500, Period: day, MaxStretch: 12, Factor: 6.65, FirstFactor: 12.57,
				Critical: []planner.CriticalJob{{Name: "too-often", Desired: 10 * time.Second, Calls: 27216}, {Name: "devices", Desired: 2 * time.Minute, Calls: 2268}},
			},
			title:  "The daily quota of account lab is too small: its jobs need at least 29,551 calls a day, it leaves 4,500.",
			action: "Raise the interval of too-often from 10s to 3m, or lower its priority to high so that Tapyr can slow it down: it makes 92% of the calls.",
		},
		{
			name:      "the other jobs alone exceed it",
			shortfall: planner.Shortfall{Needed: 7224, Budget: 4500, Period: time.Hour, MaxStretch: 12, Critical: []planner.CriticalJob{{Name: "sites"}}},
			title:     "The daily quota of account lab is too small: its jobs need at least 7,224 calls an hour, it leaves 4,500.",
			action:    "Even slowed down x12, the most Tapyr may, the jobs need more: raise their intervals or reduce their fan-out, or let Tapyr slow them down more with settings.budget.maxStretch. Critical jobs are never slowed down.",
		},
		{
			name:      "no critical job, odd window",
			shortfall: planner.Shortfall{Needed: 900, Budget: 600, Period: 30 * time.Minute, MaxStretch: 4},
			title:     "The daily quota of account lab is too small: its jobs need at least 900 calls per 30m, it leaves 600.",
			action:    "Even slowed down x4, the most Tapyr may, the jobs need more: raise their intervals or reduce their fan-out, or let Tapyr slow them down more with settings.budget.maxStretch.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := scope
			s.Shortfall = &tc.shortfall
			p := ForPlan(s, "0.5.0")
			if p.Class != ClassPlanInfeasible || p.Title != tc.title || p.Action != tc.action || p.DocsURL != "https://docs.tapyr.eu/0.5/troubleshooting/#quota-problems" {
				t.Errorf("ForPlan() =\n%s\n%s\nwant\n%s\n%s", p.Title, p.Action, tc.title, tc.action)
			}
		})
	}
	credential := planner.ScopePlan{
		Limit: "teltonika-rms/monthly", Key: "credential=34829e408ebd22e8", Accounts: []string{"fleet-rms"},
		Shortfall: &planner.Shortfall{Needed: 900, Budget: 600, Period: 30 * time.Minute, MaxStretch: 4},
	}
	if p := ForPlan(credential, "0.5.0"); !strings.HasPrefix(p.Title, "The monthly quota of account fleet-rms is too small") {
		t.Errorf("credential scope: %q", p.Title)
	}
	if p := ForPlan(scope, "0.5.0"); p != nil {
		t.Errorf("a scope without shortfall: %+v", p)
	}
	if got := shortDuration(0); got != "0s" {
		t.Errorf("shortDuration(0) = %q", got)
	}
	if got := shortDuration(2*24*time.Hour + 90*time.Second); got != "2d1m30s" {
		t.Errorf("shortDuration = %q", got)
	}
}
