// Package problem says in words what went wrong in a run and what to do
// about it: a plain sentence first, then an action, then a link to the
// section of the troubleshooting guide (spec 09, "Messages"). The technical
// error stays next to it, for those who need it.
package problem

import (
	"cmp"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/kentrow/tapyr/internal/api"
	"github.com/kentrow/tapyr/internal/budget/planner"
	"github.com/kentrow/tapyr/internal/config"
)

// Classes of a problem, in the status API.
const (
	ClassAuth           = "auth"
	ClassForbidden      = "forbidden"
	ClassReauth         = "reauth"
	ClassRateLimited    = "rate_limited"
	ClassPenalized      = "penalized"
	ClassQuotaExhausted = "quota_exhausted"
	ClassDeadline       = "deadline"
	ClassCircuitOpen    = "circuit_open"
	ClassServerError    = "server_error"
	ClassUnreachable    = "unreachable"
	ClassClient         = "client"
	ClassDecode         = "decode"
	ClassMapping        = "mapping"
	ClassUnknown        = "unknown"
)

// Sections of the troubleshooting guide (docs/troubleshooting.md), by their
// anchor.
const (
	anchorAuth     = "authentication-fails-401-403"
	anchorQuota    = "quota-problems"
	anchorSlowJobs = "jobs-run-less-often-than-configured"
	anchorProvider = "the-provider-fails-or-does-not-answer"
	anchorMetrics  = "metrics-are-wrong-or-missing"
	anchorHelp     = "asking-for-help"
)

// Input is what a problem is made of: the error details of a run (see
// state.RunRecord), the account and the references of its secrets.
type Input struct {
	// Class is the error class of the run, or reauth for credentials that
	// can no longer be renewed.
	Class   string
	Status  int
	Subject string
	Until   time.Time
	Account string
	// Secrets are the references of the secrets of the account (and of
	// its target), never their values.
	Secrets map[string]config.SecretRef
	// Version is the version of the running Tapyr, for the link to the
	// guide.
	Version string
}

// For returns the problem of a failed or skipped run. Nothing in it comes
// from the response: the technical error, which may quote it, is shown
// apart, redacted.
func For(in Input) *api.Problem {
	p := &api.Problem{Class: ClassUnknown}
	account := cmp.Or(in.Account, "the account")
	anchor := anchorHelp
	switch in.Class {
	case "auth":
		if in.Status == 403 {
			p.Class, anchor = ClassForbidden, anchorAuth
			p.Title = fmt.Sprintf("The credentials of %s lack a permission%s.", account, forSubject(in.Subject))
			p.Action = "Give them read access to it (the provider README lists the permissions Tapyr needs), then reload Tapyr."
			break
		}
		p.Class, anchor = ClassAuth, anchorAuth
		p.Title = fmt.Sprintf("The provider rejected the credentials of %s.", account)
		p.Action = fmt.Sprintf("Check %s, then reload Tapyr.", secrets(in.Secrets))
	case ClassReauth:
		p.Class, anchor = ClassReauth, anchorAuth
		p.Title = fmt.Sprintf("The credentials of %s can no longer be renewed.", account)
		p.Action = fmt.Sprintf("Create new ones as the provider README explains, update %s, then reload Tapyr.", secretsShort(in.Secrets))
	case "rate_limited":
		p.Class, anchor = ClassRateLimited, anchorQuota
		p.Title = fmt.Sprintf("The provider is rate limiting the requests of %s%s.", account, status(in.Status))
		p.Action = "Nothing to do: Tapyr backs off and tries again. If it keeps happening, another client may share the limit, or the provider definition declares it too high."
	case "penalized":
		p.Class, anchor = ClassPenalized, anchorQuota
		p.Title = fmt.Sprintf("Tapyr pauses the requests of %s: the provider asked it to wait.", account)
		p.Action = "Nothing to do: the requests resume on their own."
	case "quota_exhausted":
		p.Class, anchor = ClassQuotaExhausted, anchorQuota
		p.Title = fmt.Sprintf("The %squota of %s is used up.", nameOf(in.Subject), account)
		p.Action = "Nothing to do: the jobs resume when the quota resets. To spend less, raise intervals or lower priorities: the Plan page shows what each job spends."
	case "deadline":
		p.Class, anchor = ClassDeadline, anchorSlowJobs
		p.Title = "The run gave up waiting for its turn in the budget."
		p.Action = "Nothing to do if it is rare. If it keeps happening, the limits are too tight for the intervals: see the Plan page."
	case "circuit_open":
		p.Class, anchor = ClassCircuitOpen, anchorProvider
		p.Title = fmt.Sprintf("Tapyr paused the requests of %s after repeated failures.", account)
		p.Action = "Nothing to do: it tries again on its own after a pause. Fix the error of the failed runs if there is one."
	case "transient":
		anchor = anchorProvider
		if in.Status >= 500 {
			p.Class = ClassServerError
			p.Title = fmt.Sprintf("The provider failed to answer%s.", status(in.Status))
			p.Action = "Nothing to do: Tapyr tries again. If it lasts, check the status page of the provider."
			break
		}
		p.Class = ClassUnreachable
		p.Title = "The provider did not answer (no response, or too late)."
		p.Action = "Nothing to do: Tapyr tries again. If it lasts, check the network between Tapyr and the provider (DNS, proxy, firewall) and the status page of the provider."
	case "client":
		p.Class, anchor = ClassClient, anchorMetrics
		if in.Status == 404 {
			p.Title = fmt.Sprintf("The provider found nothing at %s%s.", cmp.Or(in.Subject, "the endpoint"), status(in.Status))
		} else {
			p.Title = fmt.Sprintf("The provider refused the request%s%s.", forSubject(in.Subject), status(in.Status))
		}
		p.Action = "Check the variables and parameters of the job (its page shows its definition). If they are right, the provider definition may be wrong: report it on the provider package."
	case "decode":
		p.Class, anchor = ClassDecode, anchorMetrics
		p.Title = fmt.Sprintf("Tapyr could not read the response%s.", forSubject(in.Subject))
		p.Action = "The API may have changed: open Live debug to see the response, and report it on the provider package."
	case ClassMapping:
		p.Class, anchor = ClassMapping, anchorMetrics
		p.Title = "Tapyr could not turn the response into metrics."
		p.Action = "Open Live debug to compare the response with the samples, and report it on the provider package."
	default:
		p.Title = "The run failed."
		p.Action = "The error below and the logs say more."
	}
	if !in.Until.IsZero() {
		until := in.Until.UTC()
		p.Until = &until
	}
	p.DocsURL = DocsURL(in.Version, anchor)
	return p
}

var release = regexp.MustCompile(`^v?(\d+)\.(\d+)\.\d+$`)

// DocsURL is the address of a section of the troubleshooting guide in the
// documentation of a version: its minor for a release (/0.5/), /dev/ for a
// release candidate or a development build, which may describe what no
// release has yet (ADR 0010).
func DocsURL(version, anchor string) string {
	path := "dev"
	if m := release.FindStringSubmatch(version); m != nil {
		path = m[1] + "." + m[2]
	}
	return "https://docs.tapyr.eu/" + path + "/troubleshooting/#" + anchor
}

// secrets names the secrets to check, by their references.
func secrets(refs map[string]config.SecretRef) string {
	if len(refs) == 0 {
		return "the secrets of the account"
	}
	names := refNames(refs)
	if len(names) == 1 {
		return "the secret " + names[0]
	}
	return "the secrets " + strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

func secretsShort(refs map[string]config.SecretRef) string {
	if len(refs) == 0 {
		return "the secrets of the account"
	}
	return strings.Join(refNames(refs), ", ")
}

// refNames returns the references of the secrets, sorted: the environment
// variable or the file, never the value.
func refNames(refs map[string]config.SecretRef) []string {
	var out []string
	for _, r := range refs {
		switch {
		case r.Env != "":
			out = append(out, r.Env+" (environment variable)")
		case r.File != "":
			out = append(out, r.File+" (file)")
		}
	}
	slices.Sort(out)
	return out
}

func forSubject(s string) string {
	if s == "" {
		return ""
	}
	return " for " + s
}

// nameOf is the name of a limit as its provider declares it, without the
// provider the budget engine prefixes it with.
func nameOf(limit string) string {
	if limit == "" {
		return ""
	}
	_, name, ok := strings.Cut(limit, "/")
	if !ok {
		name = limit
	}
	return name + " "
}

func status(code int) string {
	if code == 0 {
		return ""
	}
	return fmt.Sprintf(" (HTTP %d)", code)
}

// ClassPlanInfeasible is the class of a quota scope the plan does not fit.
const ClassPlanInfeasible = "plan_infeasible"

// maxListed bounds the critical jobs an action names.
const maxListed = 3

// ForPlan says why a quota scope does not fit the plan, and the two levers:
// slow the critical jobs down (by how much, job by job), or lower their
// priority so that the planner may. Nil for a scope that fits.
func ForPlan(s planner.ScopePlan, version string) *api.Problem {
	sf := s.Shortfall
	if sf == nil {
		return nil
	}
	p := &api.Problem{Class: ClassPlanInfeasible, DocsURL: DocsURL(version, anchorQuota)}
	p.Title = fmt.Sprintf("The %s quota%s is too small: its jobs need at least %s calls %s, it leaves %s.",
		planner.LimitName(s.Limit), planner.ScopeWords(s.Key), count(sf.Needed), period(sf), count(sf.Budget))
	switch {
	case sf.FirstFactor > 0:
		// The job with the most calls can make room on its own: the
		// simplest change.
		c := sf.Critical[0]
		p.Action = fmt.Sprintf("Raise the interval of %s from %s to %s, or lower its priority to high so that Tapyr can slow it down.",
			c.Name, shortDuration(c.Desired), shortDuration(slower(c.Desired, sf.FirstFactor)))
		if len(sf.Critical) > 1 && sf.Needed > 0 {
			p.Action = strings.TrimSuffix(p.Action, ".") + fmt.Sprintf(": it makes %.0f%% of the calls.", 100*c.Calls/sf.Needed)
		}
	case sf.Factor > 0:
		var list []string
		for _, c := range sf.Critical[:min(maxListed, len(sf.Critical))] {
			list = append(list, fmt.Sprintf("%s from %s to %s", c.Name, shortDuration(c.Desired), shortDuration(slower(c.Desired, sf.Factor))))
		}
		if more := len(sf.Critical) - maxListed; more > 0 {
			list = append(list, fmt.Sprintf("and %d more", more))
		}
		p.Action = fmt.Sprintf("Raise the intervals of the critical jobs about x%.1f (%s), or lower their priority to high so that Tapyr can slow them down.",
			math.Ceil(sf.Factor*10)/10, strings.Join(list, ", "))
	default:
		p.Action = fmt.Sprintf("Even slowed down x%g, the most Tapyr may, the jobs need more: raise their intervals or reduce their fan-out, or let Tapyr slow them down more with settings.budget.maxStretch.", sf.MaxStretch)
		if len(sf.Critical) > 0 {
			p.Action += " Critical jobs are never slowed down."
		}
	}
	return p
}

// slower returns an interval slowed down by a factor, rounded up to the
// second below a minute and to the minute above.
func slower(d time.Duration, factor float64) time.Duration {
	out := time.Duration(math.Ceil(float64(d) * factor))
	unit := time.Minute
	if out < time.Minute {
		unit = time.Second
	}
	return (out + unit - 1).Truncate(unit)
}

// period says over which time the calls count: what is left of the window
// for a plan of the running Tapyr, a full window for tapyr plan.
func period(sf *planner.Shortfall) string {
	if sf.Online {
		d := sf.Period
		if d >= time.Minute {
			d = d.Round(time.Minute)
		}
		return "over the next " + shortDuration(d)
	}
	switch sf.Period {
	case time.Minute:
		return "a minute"
	case time.Hour:
		return "an hour"
	case 24 * time.Hour:
		return "a day"
	case 7 * 24 * time.Hour:
		return "a week"
	}
	return "per " + shortDuration(sf.Period)
}

// shortDuration writes a duration the way the UI and the CLI do: 45s, 2m,
// 1h30m, 2d.
func shortDuration(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	d = d.Round(time.Second)
	var b strings.Builder
	for _, u := range []struct {
		unit time.Duration
		name string
	}{{24 * time.Hour, "d"}, {time.Hour, "h"}, {time.Minute, "m"}, {time.Second, "s"}} {
		if n := d / u.unit; n > 0 {
			fmt.Fprintf(&b, "%d%s", n, u.name)
			d -= n * u.unit
		}
	}
	return b.String()
}

// count writes a number of calls, rounded, with thousands separators.
func count(f float64) string {
	s := strconv.FormatInt(int64(math.Round(f)), 10)
	for i := len(s) - 3; i > 0 && s[i-1] != '-'; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
