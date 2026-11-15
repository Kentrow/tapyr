// Package problem says in words what went wrong in a run and what to do
// about it: a plain sentence first, then an action, then a link to the
// section of the troubleshooting guide (spec 09, "Messages"). The technical
// error stays next to it, for those who need it.
package problem

import (
	"cmp"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/kentrow/tapyr/internal/api"
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
