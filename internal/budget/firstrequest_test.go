package budget

import (
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/kentrow/tapyr/internal/clock"
)

// firstRequestAPI models a provider whose window opens with the first
// request after the previous one ended (ThousandEyes: 240 requests a minute
// per organization, "one minute from the first request in a batch"), with
// limit, remaining and reset (epoch seconds) headers.
type firstRequestAPI struct {
	limit     int
	start     time.Time
	used      int
	withReset bool
	ok        int
	rejected  int
}

func (a *firstRequestAPI) request(now time.Time) Outcome {
	if a.start.IsZero() || !now.Before(a.start.Add(time.Minute)) {
		a.start, a.used = now, 0
	}
	h := http.Header{}
	h.Set("X-Limit", strconv.Itoa(a.limit))
	if a.withReset {
		h.Set("X-Reset", strconv.FormatInt(a.start.Add(time.Minute).Unix(), 10))
	}
	if a.used >= a.limit {
		a.rejected++
		h.Set("X-Remaining", "0")
		return Outcome{Status: http.StatusTooManyRequests, Header: h}
	}
	a.used++
	a.ok++
	h.Set("X-Remaining", strconv.Itoa(a.limit-a.used))
	return Outcome{Status: http.StatusOK, Header: h}
}

// TestFirstRequestWindows checks that a fixed window that opens with the
// first request, not on the clock, is followed through the reset header of
// its responses: no 429, even for bursts sent before any response of a
// window, and the usable budget spent (#55). Without the reset header, the
// engine rolls on the clock minute while the window of the API still runs,
// and a burst right after overspends: the control case shows the model
// catches it.
func TestFirstRequestWindows(t *testing.T) {
	tests := []struct {
		name      string
		max       string // the max of the quota: fixed, or learned from the limit header
		withReset bool
		burst     int           // requests sent together, before any response
		step      time.Duration // between two bursts
		offset    time.Duration // of the first request from the clock minute
		idle      bool          // pause 50 seconds every 3 minutes: windows drift
		wantOK    bool          // no 429
	}{
		{name: "steady, aligned", max: "max: 240", withReset: true, burst: 1, step: 100 * time.Millisecond, wantOK: true},
		{name: "steady, 30s off the clock", max: "max: 240", withReset: true, burst: 1, step: 100 * time.Millisecond, offset: 30 * time.Second, wantOK: true},
		{name: "bursts of 100, 30s off", max: "max: 240", withReset: true, burst: 100, step: 10 * time.Second, offset: 30 * time.Second, wantOK: true},
		{name: "bursts of 216, 45s off", max: "max: 240", withReset: true, burst: 216, step: 15 * time.Second, offset: 45 * time.Second, wantOK: true},
		{name: "drifting windows", max: "max: 240", withReset: true, burst: 1, step: 100 * time.Millisecond, offset: 7 * time.Second, idle: true, wantOK: true},
		// The documented example shows a limit of 20 where the text says
		// 240: learned from the limit header.
		{name: "max learned from 20, 30s off", max: "max: auto\n      initialMax: 20", withReset: true, burst: 1, step: 100 * time.Millisecond, offset: 30 * time.Second, wantOK: true},
		{name: "control: bursts of 216 without the reset header", max: "max: 240", withReset: false, burst: 216, step: 15 * time.Second, offset: 45 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start := epoch.Truncate(time.Minute).Add(tt.offset)
			clk := clock.NewFake(start)
			e := newEngine(t, clk, `    - name: org
      kind: quota
      `+tt.max+`
      window: { period: minute }
      sync: { limitHeader: X-Limit, remainingHeader: X-Remaining, resetHeader: X-Reset }`, "")
			api := &firstRequestAPI{limit: 240, withReset: tt.withReset}
			end := start.Add(30 * time.Minute)
			for clk.Now().Before(end) {
				if tt.idle && clk.Now().Sub(start)%(3*time.Minute) < 50*time.Second {
					clk.Advance(tt.step)
					continue
				}
				var granted []Reservation
				for range tt.burst {
					if r, err := e.Reserve(t.Context(), desc("a")); err == nil {
						granted = append(granted, r)
					}
				}
				outcomes := make([]Outcome, len(granted))
				for i := range granted {
					outcomes[i] = api.request(clk.Now())
				}
				for i, r := range granted {
					r.Done(outcomes[i])
				}
				clk.Advance(tt.step)
			}
			if got := api.rejected == 0; got != tt.wantOK {
				t.Fatalf("%d requests rejected by the API, %d accepted", api.rejected, api.ok)
			}
			// The usable budget (240 less the 10% retry reserve) is spent:
			// at least 216 a minute while the client is active.
			minutes := 30.0
			if tt.idle {
				minutes = 30.0 * 130.0 / 180.0
			}
			if tt.wantOK && float64(api.ok)/minutes < 216 {
				t.Errorf("%d requests in %.0f minutes, want at least 216 a minute", api.ok, minutes)
			}
		})
	}
}
