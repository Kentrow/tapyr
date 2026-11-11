package budget

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kentrow/tapyr/internal/clock"
	"github.com/kentrow/tapyr/internal/config"
)

// Refusal reasons, see "Reservation algorithm" in 04-budget-engine.md.
const (
	ReasonQuotaExhausted = "quota_exhausted"
	ReasonPenalized      = "penalized"
	ReasonCircuitOpen    = "circuit_open"
	ReasonDeadline       = "deadline"
)

// RefusalError is returned when a reservation is refused.
type RefusalError struct {
	Reason string
	// Limit is the limit that refused, when there is one.
	Limit string
	Scope string
	// Until is when the refusal is expected to end, when known.
	Until time.Time
	Err   error
}

func (e *RefusalError) Error() string {
	msg := "budget refused: " + e.Reason
	if e.Limit != "" {
		msg += fmt.Sprintf(" (limit %s, scope %s)", e.Limit, e.Scope)
	}
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

func (e *RefusalError) Unwrap() error { return e.Err }

const maxPenalty = 10 * time.Minute

// Config configures an Engine.
type Config struct {
	Providers []*config.Provider
	Accounts  []*config.Account
	// RetryReserve is the fraction of every quota reserved for retries.
	RetryReserve float64
	Breaker      config.CircuitBreakerSettings
	// Observer receives the engine events (self-metrics); optional.
	Observer Observer
}

// Observer receives the events of an engine. Calls are made under the
// engine lock and must not block.
type Observer interface {
	// BudgetGranted reports the wait of a granted reservation and the limit
	// that held it last ("none" when it did not wait for a rate or
	// concurrency limit).
	BudgetGranted(provider, limit string, wait time.Duration)
	BudgetRefused(provider, account, job, reason string)
	// RateLimited reports a rate-limited response and the limit penalized
	// (the narrowest one); unexpected when no quota predicted it.
	RateLimited(provider, account, limit string, unexpected bool)
}

type nopObserver struct{}

func (nopObserver) BudgetGranted(string, string, time.Duration)  {}
func (nopObserver) BudgetRefused(string, string, string, string) {}
func (nopObserver) RateLimited(string, string, string, bool)     {}

// Engine enforces the limits of every provider. It is safe for concurrent
// use; all state is behind one lock (see "Implementation notes").
type Engine struct {
	clock  clock.Clock
	logger *slog.Logger

	limits       map[string][]*limit // provider -> limits
	errors       map[string]*config.ErrorSemantics
	shares       map[string]float64
	scaleRates   map[string]bool
	retryReserve float64
	breakerCfg   config.CircuitBreakerSettings
	observer     Observer

	mu       sync.Mutex
	states   map[string]*state // limit id + "|" + scope key
	breakers map[string]*breaker
	waiters  []*waiter
	seq      uint64
	changed  chan struct{}
	learned  chan struct{}
}

// NewEngine compiles the limits of the configuration.
func NewEngine(cfg Config, clk clock.Clock, logger *slog.Logger) (*Engine, error) {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	e := &Engine{
		clock: clk, logger: logger,
		limits: map[string][]*limit{}, errors: map[string]*config.ErrorSemantics{},
		shares: map[string]float64{}, scaleRates: map[string]bool{},
		retryReserve: cfg.RetryReserve, breakerCfg: cfg.Breaker, observer: cfg.Observer,
		states: map[string]*state{}, breakers: map[string]*breaker{},
		changed: make(chan struct{}),
		learned: make(chan struct{}),
	}
	if e.observer == nil {
		e.observer = nopObserver{}
	}
	for _, p := range cfg.Providers {
		name := string(p.Metadata.Name)
		for i := range p.Spec.Limits {
			l, err := compileLimit(name, &p.Spec.Limits[i])
			if err != nil {
				return nil, err
			}
			e.limits[name] = append(e.limits[name], l)
		}
		e.errors[name] = p.Spec.Errors
	}
	for _, a := range cfg.Accounts {
		share := 1.0
		if a.Spec.Budget != nil && a.Spec.Budget.Share != nil {
			share = *a.Spec.Budget.Share
		}
		e.shares[string(a.Metadata.Name)] = share
		e.scaleRates[string(a.Metadata.Name)] = a.Spec.Budget != nil && a.Spec.Budget.ScaleRates
	}
	return e, nil
}

// state is the state of one limit for one scope key.
type state struct {
	l        *limit
	key      string
	accounts map[string]bool

	// rate
	tokens   float64
	refilled time.Time

	// concurrency
	inUse int64

	// fixed quota window
	windowStart, windowEnd time.Time
	used                   int64
	observedMax            int64
	observed               bool
	observedRemaining      int64
	usedAtObservation      int64
	exhaustedUntil         time.Time
	mismatchLogged         bool
	// history is the consumption of the current window per bucket (minute
	// index of the bucket start -> count); prev* describe the previous
	// window, kept in memory for the status API.
	history            map[int64]int64
	prevHistory        map[int64]int64
	prevStart, prevEnd time.Time

	// rolling quota
	rolling *rollingWindow

	// 429 penalties
	penaltyUntil time.Time
	consecutive  int
}

func (e *Engine) state(l *limit, d *Descriptor, now time.Time) *state {
	key := l.scopeKey(d)
	id := l.id + "|" + key
	s, ok := e.states[id]
	if !ok {
		s = &state{l: l, key: key, accounts: map[string]bool{}, refilled: now}
		if l.kind == config.LimitRate {
			s.tokens = e.burst(s)
		}
		if l.rolling > 0 {
			s.rolling = newRollingWindow(l.rolling)
		}
		e.states[id] = s
	}
	if d.Account != "" && !s.accounts[d.Account] {
		s.accounts[d.Account] = true
		if l.kind == config.LimitRate {
			s.tokens = math.Min(s.tokens, e.burst(s))
		}
	}
	if l.fixed != nil {
		e.roll(s, now)
	}
	return s
}

// share is the smallest share among the accounts of a scope key.
func (e *Engine) share(s *state) float64 {
	share := 1.0
	for a := range s.accounts {
		if v, ok := e.shares[a]; ok {
			share = math.Min(share, v)
		}
	}
	return share
}

// scaled reports whether rate and concurrency limits of the scope are scaled
// by the share.
func (e *Engine) scaled(s *state) bool {
	for a := range s.accounts {
		if e.scaleRates[a] {
			return true
		}
	}
	return false
}

func (e *Engine) rate(s *state) float64 {
	if e.scaled(s) {
		return s.l.rate * e.share(s)
	}
	return s.l.rate
}

func (e *Engine) burst(s *state) float64 {
	if e.scaled(s) {
		return math.Max(1, math.Floor(s.l.burst*e.share(s)))
	}
	return s.l.burst
}

// headroom is the rate token that requests leave in the bucket when the
// burst allows it. Requests leave at the pace of the limit but reach the
// provider with a varying latency, and a provider bucket of the same size
// refuses one that arrives slightly early. With a token in hand, the bucket
// of the provider stays one token ahead and absorbs up to one interval of
// jitter; the sustained rate is unchanged, a burst is one request shorter.
// The enforcer (Check) applies the full burst.
func (e *Engine) headroom(s *state) float64 {
	if e.burst(s) >= 2 {
		return 1
	}
	return 0
}

func (e *Engine) concurrency(s *state) int64 {
	if e.scaled(s) {
		return max(1, int64(math.Floor(float64(s.l.max)*e.share(s))))
	}
	return s.l.max
}

// roll starts a new fixed window when the current one has ended.
func (e *Engine) roll(s *state, now time.Time) {
	if !s.windowEnd.IsZero() && now.Before(s.windowEnd) {
		return
	}
	start, end := s.l.fixed.bounds(now)
	if !s.windowEnd.IsZero() && start.Before(s.windowEnd) {
		// The provider reset earlier than the calendar (reset header): the
		// new window starts at that reset.
		start = s.windowEnd
	}
	if !s.windowEnd.IsZero() {
		s.prevHistory, s.prevStart, s.prevEnd = s.history, s.windowStart, s.windowEnd
	}
	s.history = map[int64]int64{}
	s.windowStart, s.windowEnd = start, end
	s.used, s.observed, s.observedRemaining, s.usedAtObservation = 0, false, 0, 0
	s.mismatchLogged = false
}

// quota returns the effective maximum M, the usable budget U and the retry
// reserve R of a quota state.
func (e *Engine) quota(s *state) (m, u, r int64) {
	m = s.l.max
	switch {
	case s.l.overridden:
		m = s.l.override
	case s.observedMax > 0:
		m = s.observedMax
	case s.l.auto:
		m = s.l.initialMax
	}
	u = int64(math.Floor(float64(m) * e.share(s)))
	r = int64(math.Floor(float64(u) * e.retryReserve))
	return m, u, r
}

func (e *Engine) quotaUsed(s *state, now time.Time) int64 {
	if s.rolling != nil {
		return s.rolling.used(now)
	}
	return s.used
}

// quotaAvailable reports whether one more unit may be consumed, and when
// the quota is expected to become available again otherwise.
func (e *Engine) quotaAvailable(s *state, d *Descriptor, now time.Time) (bool, time.Time) {
	end := s.windowEnd
	if s.rolling != nil {
		end = s.rolling.nextExpiry(now)
	}
	if now.Before(s.exhaustedUntil) {
		return false, s.exhaustedUntil
	}
	_, u, r := e.quota(s)
	allowed := u - r
	if d.Retry || d.Priority == config.PriorityCritical {
		allowed = u
	}
	used := e.quotaUsed(s, now)
	if used >= allowed {
		return false, end
	}
	if s.observed && s.observedRemaining-(used-s.usedAtObservation) <= 0 {
		return false, end
	}
	return true, time.Time{}
}

func (e *Engine) refill(s *state, now time.Time) {
	if elapsed := now.Sub(s.refilled).Seconds(); elapsed > 0 {
		s.tokens = math.Min(e.burst(s), s.tokens+elapsed*e.rate(s))
	}
	s.refilled = now
}

type waiter struct {
	seq      uint64
	priority int
	keys     []string
	// since is the first attempt; heldBy the limit that made it wait last.
	since  time.Time
	heldBy string
}

func priorityRank(p config.Priority) int {
	switch p {
	case config.PriorityCritical:
		return 3
	case config.PriorityHigh:
		return 2
	case config.PriorityLow:
		return 0
	}
	return 1
}

// blocked reports whether a waiter ahead of w (higher priority, or same
// priority and older) waits on one of its keys.
func (e *Engine) blocked(w *waiter) bool {
	for _, o := range e.waiters {
		if o == w {
			continue
		}
		ahead := o.priority > w.priority || (o.priority == w.priority && o.seq < w.seq)
		if ahead && slices.ContainsFunc(o.keys, func(k string) bool { return slices.Contains(w.keys, k) }) {
			return true
		}
	}
	return false
}

func (e *Engine) removeWaiter(w *waiter) {
	if i := slices.Index(e.waiters, w); i >= 0 {
		e.waiters = slices.Delete(e.waiters, i, i+1)
		e.notifyLocked()
	}
}

func (e *Engine) notifyLocked() {
	close(e.changed)
	e.changed = make(chan struct{})
}

// Reserve grants a reservation once every applicable limit allows the
// request, waiting for rate tokens, concurrency slots and penalties.
func (e *Engine) Reserve(ctx context.Context, d Descriptor) (Reservation, error) {
	var w *waiter
	for {
		if err := ctx.Err(); err != nil {
			return nil, e.refuse(&d, w, &RefusalError{Reason: ReasonDeadline, Err: err})
		}
		e.mu.Lock()
		now := e.clock.Now()
		res, wake, err := e.try(&d, now, &w)
		if err != nil || res != nil {
			e.mu.Unlock()
			return res, err
		}
		changed := e.changed
		e.mu.Unlock()

		var timer clock.Timer
		var timerC <-chan time.Time
		if !wake.IsZero() {
			timer = e.clock.NewTimer(wake.Sub(now))
			timerC = timer.C()
		}
		select {
		case <-ctx.Done():
		case <-changed:
		case <-timerC:
		}
		if timer != nil {
			timer.Stop()
		}
	}
}

// refuse records a refusal and unregisters the waiter.
func (e *Engine) refuse(d *Descriptor, w *waiter, err *RefusalError) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if w != nil {
		e.removeWaiter(w)
	}
	e.observer.BudgetRefused(d.Provider, d.Account, d.Job, err.Reason)
	return err
}

// try attempts the reservation under the lock. It returns the reservation,
// or the time to wake up at (zero: wait for a release), or a refusal.
func (e *Engine) try(d *Descriptor, now time.Time, wp **waiter) (Reservation, time.Time, error) {
	refuse := func(err *RefusalError) (Reservation, time.Time, error) {
		if *wp != nil {
			e.removeWaiter(*wp)
		}
		e.observer.BudgetRefused(d.Provider, d.Account, d.Job, err.Reason)
		return nil, time.Time{}, err
	}
	b := e.breaker(breakerKey(d))
	probe, ok := b.allow(now)
	if !ok {
		return refuse(&RefusalError{Reason: ReasonCircuitOpen, Until: b.openUntil})
	}

	var states []*state
	for _, l := range e.limits[d.Provider] {
		if l.applies(d) {
			states = append(states, e.state(l, d, now))
		}
	}
	var wake time.Time
	waitFor := func(t time.Time) {
		if wake.IsZero() || t.Before(wake) {
			wake = t
		}
	}
	mustWait, heldBy := false, ""
	for _, s := range states {
		if now.Before(s.penaltyUntil) {
			if !d.Deadline.IsZero() && s.penaltyUntil.After(d.Deadline) {
				return refuse(&RefusalError{Reason: ReasonPenalized, Limit: s.l.id, Scope: s.key, Until: s.penaltyUntil})
			}
			mustWait = true
			waitFor(s.penaltyUntil)
		}
		switch s.l.kind {
		case config.LimitQuota:
			if ok, until := e.quotaAvailable(s, d, now); !ok {
				return refuse(&RefusalError{Reason: ReasonQuotaExhausted, Limit: s.l.id, Scope: s.key, Until: until})
			}
		case config.LimitConcurrency:
			if s.inUse >= e.concurrency(s) {
				mustWait, heldBy = true, s.l.name
			}
		case config.LimitRate:
			e.refill(s, now)
			if need := 1 + e.headroom(s); s.tokens < need {
				at := now.Add(time.Duration((need - s.tokens) / e.rate(s) * float64(time.Second)))
				if !d.Deadline.IsZero() && at.After(d.Deadline) {
					return refuse(&RefusalError{Reason: ReasonDeadline, Limit: s.l.id, Scope: s.key, Until: at})
				}
				mustWait, heldBy = true, s.l.name
				waitFor(at)
			}
		}
	}
	if *wp == nil {
		e.seq++
		w := &waiter{seq: e.seq, priority: priorityRank(d.Priority), since: now, heldBy: "none"}
		for _, s := range states {
			w.keys = append(w.keys, s.l.id+"|"+s.key)
		}
		*wp = w
	}
	w := *wp
	if heldBy != "" {
		w.heldBy = heldBy
	}
	if !mustWait && !e.blocked(w) {
		for _, s := range states {
			switch s.l.kind {
			case config.LimitQuota:
				if s.rolling != nil {
					s.rolling.add(now, 1)
				} else {
					s.used++
					s.record(now)
				}
			case config.LimitConcurrency:
				s.inUse++
			case config.LimitRate:
				s.tokens--
			}
		}
		e.removeWaiter(w)
		e.observer.BudgetGranted(d.Provider, w.heldBy, now.Sub(w.since))
		if probe {
			b.probing = true
		}
		return &reservation{e: e, d: *d, states: states, probe: probe, at: now}, time.Time{}, nil
	}
	if !d.Deadline.IsZero() {
		waitFor(d.Deadline)
		if !now.Before(d.Deadline) {
			return refuse(&RefusalError{Reason: ReasonDeadline})
		}
	}
	if !slices.Contains(e.waiters, w) {
		e.waiters = append(e.waiters, w)
	}
	return nil, wake, nil
}

type reservation struct {
	e      *Engine
	d      Descriptor
	states []*state
	probe  bool
	at     time.Time // grant time, for tests
	once   sync.Once
}

// Done releases the reservation and applies the outcome: header sync,
// penalties and circuit breaker.
func (r *reservation) Done(o Outcome) {
	r.once.Do(func() { r.e.done(r, o) })
}

func (e *Engine) done(r *reservation, o Outcome) {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.clock.Now()
	for _, s := range r.states {
		if s.l.kind == config.LimitConcurrency {
			s.inUse--
		}
	}
	if o.Header != nil {
		for _, s := range r.states {
			if s.l.kind == config.LimitQuota && s.l.sync != nil {
				e.syncHeaders(s, o.Header, now)
			}
		}
	}
	e.handleRateLimited(r, o, now)
	e.breaker(breakerKey(&r.d)).record(o, r.probe, now, e.breakerCfg)
	e.notifyLocked()
}

// syncHeaders applies the quota headers of a response.
func (e *Engine) syncHeaders(s *state, h http.Header, now time.Time) {
	sync := s.l.sync
	if v, ok := headerInt(h, sync.LimitHeader); ok && v > 0 {
		if !s.l.auto && v != s.l.max && !s.mismatchLogged {
			s.mismatchLogged = true
			e.logger.Warn("quota limit header disagrees with the declared max", "component", "budget",
				"provider", s.l.provider, "limit", s.l.name, "declared", s.l.max, "header", v)
		}
		if s.observedMax != v {
			close(e.learned)
			e.learned = make(chan struct{})
		}
		s.observedMax = v
	}
	if v, ok := headerInt(h, sync.RemainingHeader); ok {
		s.observed, s.observedRemaining, s.usedAtObservation = true, v, e.quotaUsed(s, now)
		if v <= 0 {
			s.exhaustedUntil = s.windowEnd
		}
	}
	if v, ok := headerInt(h, sync.ResetHeader); ok && v > 0 {
		reset := now.Add(time.Duration(v) * time.Second)
		if v > 1e9 {
			reset = time.Unix(v, 0)
		}
		if reset.After(now) && s.rolling == nil {
			s.windowEnd = reset
			if !s.exhaustedUntil.IsZero() {
				s.exhaustedUntil = reset
			}
		}
	}
}

func headerInt(h http.Header, name string) (int64, bool) {
	if name == "" {
		return 0, false
	}
	v := strings.TrimSpace(h.Get(name))
	if v == "" {
		return 0, false
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		f, ferr := strconv.ParseFloat(v, 64)
		if ferr != nil {
			return 0, false
		}
		n = int64(f)
	}
	return n, true
}

// handleRateLimited applies a penalty after a rate-limited response.
func (e *Engine) handleRateLimited(r *reservation, o Outcome, now time.Time) {
	sem := e.errors[r.d.Provider]
	if sem == nil || sem.RateLimited == nil || !slices.Contains(sem.RateLimited.Statuses, o.Status) {
		for _, s := range r.states {
			s.consecutive = 0
		}
		return
	}
	predicted := false
	for _, s := range r.states {
		if s.l.kind == config.LimitQuota {
			if ok, _ := e.quotaAvailable(s, &r.d, now); !ok {
				predicted = true
			}
		}
	}
	targets, penalized := r.states, "none"
	if len(r.states) > 0 {
		narrowest := r.states[0]
		for _, s := range r.states[1:] {
			if s.l.narrowness() > narrowest.l.narrowness() {
				narrowest = s
			}
		}
		penalized = narrowest.l.name
		if sem.RateLimited.Penalize != "all" {
			targets = []*state{narrowest}
		}
	}
	e.observer.RateLimited(r.d.Provider, r.d.Account, penalized, !predicted)
	retryAfter, ok := parseRetryAfter(o.Header, sem.RateLimited.RetryAfterHeader, now)
	for _, s := range targets {
		s.consecutive++
		penalty := retryAfter
		if !ok {
			penalty = min(maxPenalty, sem.RateLimited.DefaultPenalty.Std()<<(s.consecutive-1))
		}
		s.penaltyUntil = now.Add(penalty)
	}
	e.logger.Info("rate limited", "component", "budget", "provider", r.d.Provider, "account", r.d.Account,
		"endpoint", r.d.Endpoint, "unexpected", !predicted)
}

// parseRetryAfter reads a Retry-After header: seconds or an HTTP date.
func parseRetryAfter(h http.Header, name string, now time.Time) (time.Duration, bool) {
	if h == nil || name == "" {
		return 0, false
	}
	v := strings.TrimSpace(h.Get(name))
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.ParseFloat(v, 64); err == nil && secs >= 0 {
		return min(maxPenalty, time.Duration(secs*float64(time.Second))), true
	}
	if t, err := http.ParseTime(v); err == nil {
		return min(maxPenalty, max(0, t.Sub(now))), true
	}
	return 0, false
}

// LimitLearned returns a channel closed the next time a response header
// changes the effective max of a quota, a trigger for replanning.
func (e *Engine) LimitLearned() <-chan struct{} {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.learned
}

// IsRefusal reports whether err is a budget refusal, and its reason.
func IsRefusal(err error) (string, bool) {
	if re, ok := errors.AsType[*RefusalError](err); ok {
		return re.Reason, true
	}
	return "", false
}
