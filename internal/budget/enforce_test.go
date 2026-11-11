package budget

import (
	"errors"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/kentrow/tapyr/internal/clock"
	"github.com/kentrow/tapyr/internal/config"
)

func TestCheckEnforcer(t *testing.T) {
	clk := clock.NewFake(epoch)
	e := newEngine(t, clk, `    - { name: rate, kind: rate, rate: 2/s, burst: 2, scope: { by: [credential] } }
    - { name: conc, kind: concurrency, max: 1, scope: { by: [credential] } }
    - { name: daily, kind: quota, max: 3, window: { period: day }, scope: { by: [credential] } }`, "")
	d := Descriptor{Provider: "p", Credential: "k", Endpoint: "/x"}

	release, refused := e.Check(d)
	if refused != nil {
		t.Fatal(refused)
	}
	if _, r := e.Check(d); r == nil || r.Reason != "concurrency" {
		t.Fatalf("second concurrent request: %v", r)
	}
	release()
	release2, _ := e.Check(d)
	release2()
	// Burst of 2 used: the next token comes in 500ms.
	if _, r := e.Check(d); r == nil || r.Reason != "rate" || !r.Until.Equal(epoch.Add(500*time.Millisecond)) {
		t.Fatalf("rate refusal: %+v", r)
	}
	clk.Advance(time.Second)
	release3, _ := e.Check(d)
	release3()
	// The enforcer applies the full quota of 3 (no retry reserve).
	if _, r := e.Check(d); r == nil || r.Reason != ReasonQuotaExhausted {
		t.Fatalf("quota refusal: %+v", r)
	}

	e.SetQuotaMax("p", "daily", 5)
	if q, ok := e.QuotaOf(d, "daily"); !ok || q.Max != 5 || q.Remaining != 2 {
		t.Fatalf("after override: %+v %v", q, ok)
	}
	e.Consume("p", "daily", 2)
	if q, _ := e.QuotaOf(d, "daily"); q.Used != 5 || q.Remaining != 0 {
		t.Fatalf("after external consumption: %+v", q)
	}
	e.SetQuotaMax("p", "daily", -1)
	if q, _ := e.QuotaOf(d, "daily"); q.Max != 3 {
		t.Fatalf("override not removed: %+v", q)
	}
	if _, ok := e.QuotaOf(d, "nope"); ok {
		t.Error("QuotaOf of an unknown limit")
	}
}

func TestRollingConsume(t *testing.T) {
	clk := clock.NewFake(epoch)
	e := newEngine(t, clk, "    - { name: hourly, kind: quota, max: 10, window: { rolling: 1h } }", "")
	d := desc("a")
	reserve(t, e, d).Done(Outcome{})
	e.Consume("p", "hourly", 4)
	if q, _ := e.QuotaOf(d, "hourly"); q.Used != 5 || !q.Rolling {
		t.Fatalf("rolling quota: %+v", q)
	}
	if entries := e.Export(); len(entries) != 1 || entries[0].Minutes[minuteIndex(epoch)] != 5 {
		t.Fatalf("export = %+v", entries)
	}
}

func TestPlanScopes(t *testing.T) {
	clk := clock.NewFake(epoch)
	e := newEngine(t, clk, `    - { name: org-rate, kind: rate, rate: 10/s, scope: { by: [account, "var:orgId"] }, appliesTo: { requestsWithVar: orgId } }
    - { name: rate, kind: rate, rate: 100/s }
    - { name: conc, kind: concurrency, max: 2 }
    - { name: daily, kind: quota, max: 1000, window: { period: day }, scope: { by: [account] } }
    - { name: hourly, kind: quota, max: 100, window: { rolling: 1h } }`, "")
	scopes := e.PlanScopes(Descriptor{Provider: "p", Account: "b", Endpoint: "/orgs/{orgId}/devices"}, []string{"orgId"})
	byLimit := map[string]PlanScope{}
	for _, s := range scopes {
		byLimit[s.Limit] = s
	}
	if len(scopes) != 4 || byLimit["p/conc"].ID != "" {
		t.Fatalf("scopes = %+v", scopes)
	}
	if s := byLimit["p/org-rate"]; !s.PerInstance || s.Rate != 10 || s.Key != "account=b,orgId=*" {
		t.Errorf("per-instance rate scope = %+v", s)
	}
	if s := byLimit["p/daily"]; s.Usable != 500 || s.Reserve != 50 || s.WindowLength != 24*time.Hour || s.PerInstance {
		t.Errorf("daily scope = %+v (share 0.5)", s)
	}
	if s := byLimit["p/hourly"]; !s.Rolling || s.WindowLength != time.Hour {
		t.Errorf("rolling scope = %+v", s)
	}
	none := e.PlanScopes(Descriptor{Provider: "p", Account: "a", Endpoint: "/x"}, nil)
	if slices.ContainsFunc(none, func(s PlanScope) bool { return s.Limit == "p/org-rate" }) {
		t.Error("org-rate applies without its variable")
	}
}

func TestScaledRates(t *testing.T) {
	clk := clock.NewFake(epoch)
	e := newEngine(t, clk, `    - { name: rate, kind: rate, rate: 4/s, burst: 8, scope: { by: [account] } }
    - { name: conc, kind: concurrency, max: 4, scope: { by: [account] } }`, `---
apiVersion: tapyr/v1alpha1
kind: Account
metadata: { name: scaled }
spec: { provider: p, budget: { share: 0.5, scaleRates: true } }
`)
	d := desc("scaled")
	r1, r2 := reserve(t, e, d), reserve(t, e, d)
	ch := async(t.Context(), e, d)
	pending(t, ch) // concurrency 4 x 0.5 = 2
	r1.Done(Outcome{})
	r2.Done(Outcome{})
	if err := completed(t, ch); err != nil {
		t.Fatal(err)
	}
	// burst 8 x 0.5 = 4, three used and one kept in hand: the fourth waits
	// for a token, refilled at 4 x 0.5 = 2/s.
	ch = async(t.Context(), e, d)
	pending(t, ch)
	clk.Advance(500 * time.Millisecond)
	if err := completed(t, ch); err != nil {
		t.Fatal(err)
	}
	if scopes := e.PlanScopes(d, nil); scopes[0].Rate != 2 {
		t.Errorf("scaled rate = %v", scopes[0].Rate)
	}
}

func TestLimitLearnedAndErrors(t *testing.T) {
	clk := clock.NewFake(epoch)
	e := newEngine(t, clk, `    - name: daily
      kind: quota
      max: 100
      window: { period: day }
      sync: { limitHeader: X-Limit }`, "")
	learned := e.LimitLearned()
	reserve(t, e, desc("a")).Done(Outcome{Status: 200, Header: http.Header{"X-Limit": {"250.0"}}})
	select {
	case <-learned:
	default:
		t.Fatal("LimitLearned not signaled")
	}
	if q := e.Quotas()[0]; q.Max != 250 {
		t.Errorf("max = %d", q.Max)
	}
	reserve(t, e, desc("a")).Done(Outcome{Status: 200, Header: http.Header{"X-Limit": {"garbage"}}})
	if q := e.Quotas()[0]; q.Max != 250 {
		t.Errorf("garbage header changed the max: %d", q.Max)
	}

	err := error(&RefusalError{Reason: ReasonQuotaExhausted, Limit: "p/daily", Scope: "instance", Err: errors.New("boom")})
	if err.Error() != "budget refused: quota_exhausted (limit p/daily, scope instance): boom" {
		t.Errorf("Error() = %q", err.Error())
	}
	if _, ok := IsRefusal(errors.New("other")); ok {
		t.Error("IsRefusal on another error")
	}
	if !GlobMatch("/a/*", "/a/b") || matchSegment("x*y*z", "xaybz") != true || matchSegment("x*y", "xa") {
		t.Error("glob segments")
	}
	_ = config.LimitQuota
}
