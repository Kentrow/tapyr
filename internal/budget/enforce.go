package budget

import (
	"time"

	"github.com/kentrow/tapyr/internal/config"
)

// Check is the enforcer mode used by the simulator: it decides at once,
// without waiting, whether a request is allowed. When it is, the limits are
// acquired and release must be called once the response is sent. When it is
// not, the refusal tells how long the client should wait (Retry-After).
//
// The enforcer applies the full usable budget (no retry reserve: providers
// do not keep one for their clients).
func (e *Engine) Check(d Descriptor) (release func(), refused *RefusalError) {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.clock.Now()
	d.Retry = true // full budget, see above
	var states []*state
	for _, l := range e.limits[d.Provider] {
		if l.applies(&d) {
			states = append(states, e.state(l, &d, now))
		}
	}
	for _, s := range states {
		switch s.l.kind {
		case config.LimitQuota:
			if ok, until := e.quotaAvailable(s, &d, now); !ok {
				return nil, &RefusalError{Reason: ReasonQuotaExhausted, Limit: s.l.id, Scope: s.key, Until: until}
			}
		case config.LimitConcurrency:
			if s.inUse >= e.concurrency(s) {
				return nil, &RefusalError{Reason: "concurrency", Limit: s.l.id, Scope: s.key, Until: now.Add(time.Second)}
			}
		case config.LimitRate:
			e.refill(s, now)
			if s.tokens < 1 {
				at := now.Add(time.Duration((1 - s.tokens) / e.rate(s) * float64(time.Second)))
				return nil, &RefusalError{Reason: "rate", Limit: s.l.id, Scope: s.key, Until: at}
			}
		}
	}
	for _, s := range states {
		switch s.l.kind {
		case config.LimitQuota:
			if s.rolling != nil {
				s.rolling.add(now, 1)
			} else {
				s.used++
			}
		case config.LimitConcurrency:
			s.inUse++
		case config.LimitRate:
			s.tokens--
		}
	}
	return func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		for _, s := range states {
			if s.l.kind == config.LimitConcurrency {
				s.inUse--
			}
		}
	}, nil
}

// SetQuotaMax overrides the declared max of a quota limit of a provider
// (simulator scenarios); a negative max restores the declaration.
func (e *Engine) SetQuotaMax(provider, limitName string, maxPerWindow int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, l := range e.limits[provider] {
		if l.name != limitName || l.kind != config.LimitQuota {
			continue
		}
		if maxPerWindow < 0 {
			l.override = 0
			l.overridden = false
		} else {
			l.override, l.overridden = maxPerWindow, true
		}
	}
}

// Consume adds n units to every known scope of a quota limit (simulated
// external consumption).
func (e *Engine) Consume(provider, limitName string, n int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.clock.Now()
	for _, s := range e.states {
		if s.l.provider != provider || s.l.name != limitName || s.l.kind != config.LimitQuota {
			continue
		}
		if s.l.fixed != nil {
			e.roll(s, now)
		}
		if s.rolling != nil {
			s.rolling.add(now, n)
		} else {
			s.used += n
		}
	}
}

// QuotaWindowStart returns the start of the current window of a quota
// limit: the latest of its scopes for a fixed window, the start of the
// period for a rolling one; zero before any request. The simulator charges
// the external consumption of a scenario from there: what was consumed
// before belongs to windows that have reset since.
func (e *Engine) QuotaWindowStart(provider, limitName string) time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.clock.Now()
	var start time.Time
	for _, s := range e.states {
		if s.l.provider != provider || s.l.name != limitName || s.l.kind != config.LimitQuota {
			continue
		}
		var ws time.Time
		switch {
		case s.l.fixed != nil:
			e.roll(s, now)
			ws = s.windowStart
		case s.rolling != nil:
			ws = now.Add(-time.Duration(s.rolling.minutes) * time.Minute)
		}
		if ws.After(start) {
			start = ws
		}
	}
	return start
}

// QuotaOf returns the state of a quota limit for the scope of a request.
func (e *Engine) QuotaOf(d Descriptor, limitName string) (QuotaState, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.clock.Now()
	for _, l := range e.limits[d.Provider] {
		if l.name == limitName && l.kind == config.LimitQuota && l.applies(&d) {
			return e.quotaState(e.state(l, &d, now), now), true
		}
	}
	return QuotaState{}, false
}
