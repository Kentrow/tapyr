// Package push is the engine shared by the push outputs (remote write and
// OTLP): a bounded queue of points, batching, and a sender loop retrying
// with backoff (see "Implementation notes" in
// docs/spec/06-mapping-and-outputs.md).
package push

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kentrow/tapyr/internal/clock"
	"github.com/kentrow/tapyr/internal/model"
	"github.com/kentrow/tapyr/internal/output"
)

// Point is one queued data point: a sample, or the staleness marker of a
// series (Stale, with the time the series went stale as Timestamp and its
// type when known).
type Point struct {
	model.Sample
	Stale bool
	// Provider and Account of the job instance, for the OTLP resource.
	Provider, Account string
}

// Result is the outcome of an accepted send.
type Result struct {
	// Rejected counts points the backend refused in a partial success.
	Rejected int
	// Message explains the rejection.
	Message string
}

// Sender sends a batch of points to the backend.
type Sender interface {
	Send(ctx context.Context, points []Point) (Result, error)
}

// Send error classes; transient and throttled are retried.
const (
	ClassTransient  = "transient"
	ClassThrottled  = "throttled"
	ClassRejected   = "rejected"
	ClassOutOfOrder = "out_of_order"
)

// Error is a failed send.
type Error struct {
	Class string
	// RetryAfter is the delay asked by the backend, zero when none.
	RetryAfter time.Duration
	Status     int
	Err        error
}

func (e *Error) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("%s: HTTP %d: %v", e.Class, e.Status, e.Err)
	}
	return fmt.Sprintf("%s: %v", e.Class, e.Err)
}

func (e *Error) Unwrap() error { return e.Err }

// Retryable reports whether the batch is sent again.
func (e *Error) Retryable() bool { return e.Class == ClassTransient || e.Class == ClassThrottled }

// maxRetryAfter caps the delay a backend may ask for.
const maxRetryAfter = 5 * time.Minute

// Backoff of retried sends: full jitter, doubling from 1s up to 1m.
const (
	initialBackoff = time.Second
	maxBackoff     = time.Minute
)

// maxSplitSends bounds the requests spent on a batch refused for out of
// order samples: the batch is split in halves, sent again, and the refused
// halves split further down to the points really refused, as long as the
// requests of the batch stay under this bound.
const maxSplitSends = 64

// HTTPError classifies an HTTP response: nil for 2xx; retryable reports
// whether a status other than 429 is retried (5xx for remote write, 502,
// 503 and 504 for OTLP). Rejections mentioning out of order, too old or
// out of bounds samples are out_of_order.
func HTTPError(resp *http.Response, body []byte, now time.Time, retryable func(status int) bool) *Error {
	status := resp.StatusCode
	if status >= 200 && status < 300 {
		return nil
	}
	msg := strings.TrimSpace(string(body))
	if status >= 300 && status < 400 {
		// Outputs do not follow redirects: their headers may hold
		// credentials. The endpoint of the output must be the final URL.
		msg = "redirected (outputs do not follow redirects: set the final URL of the endpoint)"
		if u, err := resp.Location(); err == nil {
			msg = "redirected to " + u.Host + " (outputs do not follow redirects: set the final URL of the endpoint)"
		}
	}
	if len(msg) > 256 {
		msg = msg[:256] + "..."
	}
	e := &Error{Status: status, Err: errors.New(msg), RetryAfter: retryAfter(resp.Header.Get("Retry-After"), now)}
	lower := strings.ToLower(msg)
	switch {
	case status == http.StatusTooManyRequests:
		e.Class = ClassThrottled
	case retryable(status):
		e.Class = ClassTransient
	case strings.Contains(lower, "out of order") || strings.Contains(lower, "out-of-order") ||
		strings.Contains(lower, "too old") || strings.Contains(lower, "out of bounds"):
		e.Class = ClassOutOfOrder
	default:
		e.Class = ClassRejected
	}
	return e
}

func retryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.ParseFloat(v, 64); err == nil && secs > 0 {
		return min(maxRetryAfter, time.Duration(secs*float64(time.Second)))
	}
	if t, err := http.ParseTime(v); err == nil {
		return min(maxRetryAfter, max(0, t.Sub(now)))
	}
	return 0
}

// Observer receives the events of a queue (self-metrics).
type Observer interface {
	OutputSent(output string, n int)
	OutputDropped(output, reason string, n int)
	OutputSendFailed(output, class string)
	OutputSendDuration(output string, d time.Duration)
}

type nopObserver struct{}

func (nopObserver) OutputSent(string, int)                   {}
func (nopObserver) OutputDropped(string, string, int)        {}
func (nopObserver) OutputSendFailed(string, string)          {}
func (nopObserver) OutputSendDuration(string, time.Duration) {}

// Options configures a Queue.
type Options struct {
	// Name of the output, for logs and self-metrics.
	Name string
	// Capacity bounds the queue, in points.
	Capacity int
	// MaxBatch bounds the points of a send.
	MaxBatch int
	// FlushInterval sends a partial batch once its oldest point waited
	// that long.
	FlushInterval time.Duration
	Sender        Sender
	Clock         clock.Clock
	Logger        *slog.Logger
	Observer      Observer
	// Rand draws the backoff jitter; inject a seeded source in tests.
	Rand *rand.Rand
}

// Queue queues points and sends them in batches. One send is in flight at
// a time, so that the points of a series leave in order. It is safe for
// concurrent use.
type Queue struct {
	opts Options

	mu      sync.Mutex
	points  []entry
	seq     uint64
	sending uint64 // last seq of the batch in flight, 0 when none
	attempt int    // consecutive failed attempts
	flushes int    // Flush calls waiting
	drained chan struct{}
	wake    chan struct{}
	state   output.State

	// The batch refused for out of order samples, being split: the spans
	// left to send, in order, its last seq and the requests spent on it.
	spans      []span
	splitLast  uint64
	splitSends int
}

type entry struct {
	Point
	seq uint64
	at  time.Time
}

// span is a range of seqs of the batch being split, bounds included.
type span struct{ first, last uint64 }

// New returns a Queue.
func New(opts Options) *Queue {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Observer == nil {
		opts.Observer = nopObserver{}
	}
	if opts.Rand == nil {
		opts.Rand = rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64())) //nolint:gosec // G404: backoff jitter needs no cryptographic randomness.
	}
	opts.Capacity, opts.MaxBatch = max(1, opts.Capacity), max(1, opts.MaxBatch)
	return &Queue{
		opts: opts, drained: make(chan struct{}), wake: make(chan struct{}, 1),
		state: output.State{Capacity: opts.Capacity},
	}
}

// Add queues points without blocking. When the queue is full, the oldest
// points are dropped (queue_full), except the batch being sent.
func (q *Queue) Add(points []Point) {
	if len(points) == 0 {
		return
	}
	q.mu.Lock()
	now := q.opts.Clock.Now()
	for _, p := range points {
		q.seq++
		q.points = append(q.points, entry{Point: p, seq: q.seq, at: now})
	}
	dropped := 0
	if over := len(q.points) - q.opts.Capacity; over > 0 {
		// Points of the batch in flight stay: they are being delivered.
		keep := 0
		for keep < len(q.points) && q.points[keep].seq <= q.sending {
			keep++
		}
		dropped = min(over, len(q.points)-keep)
		q.points = append(q.points[:keep], q.points[keep+dropped:]...)
		q.state.Dropped += int64(dropped)
	}
	q.state.Queue = len(q.points)
	q.mu.Unlock()
	if dropped > 0 {
		q.opts.Observer.OutputDropped(q.opts.Name, "queue_full", dropped)
		q.opts.Logger.Warn("output queue full: oldest samples dropped", "component", "output", "output", q.opts.Name,
			"dropped", dropped, "capacity", q.opts.Capacity)
	}
	q.signal()
}

func (q *Queue) signal() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// State describes the queue and the sends.
func (q *Queue) State() output.State {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.state
}

// Run sends batches until ctx is done.
func (q *Queue) Run(ctx context.Context) error {
	for {
		batch, wait := q.next()
		if batch == nil {
			var timerC <-chan time.Time
			var timer clock.Timer
			if wait > 0 {
				timer = q.opts.Clock.NewTimer(wait)
				timerC = timer.C()
			}
			select {
			case <-ctx.Done():
				if timer != nil {
					timer.Stop()
				}
				return nil
			case <-q.wake:
			case <-timerC:
			}
			if timer != nil {
				timer.Stop()
			}
			continue
		}
		if delay := q.send(ctx, batch); delay > 0 {
			if q.opts.Clock.Sleep(ctx, delay) != nil {
				return nil
			}
		}
	}
}

// next returns the batch to send now, or how long to wait for one (0: until
// points are added).
func (q *Queue) next() ([]entry, time.Duration) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.points) == 0 {
		return nil, 0
	}
	if batch := q.nextSpan(); batch != nil {
		return batch, 0
	}
	now := q.opts.Clock.Now()
	due := q.points[0].at.Add(q.opts.FlushInterval)
	if len(q.points) < q.opts.MaxBatch && q.flushes == 0 && now.Before(due) {
		return nil, due.Sub(now)
	}
	batch := make([]entry, min(len(q.points), q.opts.MaxBatch))
	copy(batch, q.points)
	q.sending = batch[len(batch)-1].seq
	return batch, 0
}

// nextSpan returns the points of the next span of the batch being split,
// nil when no batch is split; q.mu must be held.
func (q *Queue) nextSpan() []entry {
	for len(q.spans) > 0 {
		sp := q.spans[0]
		var batch []entry
		for _, e := range q.points {
			if e.seq > sp.last {
				break
			}
			if e.seq >= sp.first {
				batch = append(batch, e)
			}
		}
		if len(batch) > 0 {
			q.sending = q.splitLast
			return batch
		}
		q.spans = q.spans[1:] // its points were dropped meanwhile
	}
	q.splitLast, q.splitSends = 0, 0
	return nil
}

// split replaces the span of batch, refused for out of order samples, by
// its halves; it reports false when batch is one point or the requests of
// the batch reached maxSplitSends. q.mu must be held.
func (q *Queue) split(batch []entry) bool {
	if len(batch) < 2 || q.splitSends >= maxSplitSends {
		return false
	}
	mid := len(batch) / 2
	halves := []span{{batch[0].seq, batch[mid-1].seq}, {batch[mid].seq, batch[len(batch)-1].seq}}
	if len(q.spans) == 0 {
		q.spans, q.splitLast = halves, batch[len(batch)-1].seq
	} else {
		q.spans = append(halves, q.spans[1:]...)
	}
	q.sending = q.splitLast
	return true
}

// done removes the points of a batch delivered or dropped, up to seq; q.mu
// must be held.
func (q *Queue) done(seq uint64) {
	if len(q.spans) > 0 {
		q.spans = q.spans[1:]
	}
	q.remove(seq)
	if len(q.spans) > 0 {
		q.sending = q.splitLast
	} else {
		q.splitLast, q.splitSends = 0, 0
	}
}

// send sends one batch and returns the delay before the next attempt when
// it must be retried.
func (q *Queue) send(ctx context.Context, batch []entry) time.Duration {
	points := make([]Point, len(batch))
	for i := range batch {
		points[i] = batch[i].Point
	}
	started := q.opts.Clock.Now()
	res, err := q.opts.Sender.Send(ctx, points)
	now := q.opts.Clock.Now()
	q.opts.Observer.OutputSendDuration(q.opts.Name, now.Sub(started))
	last := batch[len(batch)-1].seq
	q.mu.Lock()
	if len(q.spans) > 0 {
		q.splitSends++
	}
	q.mu.Unlock()
	if err == nil {
		sent := len(batch) - res.Rejected
		q.mu.Lock()
		q.done(last)
		q.attempt = 0
		q.state.Sent += int64(sent)
		q.state.Dropped += int64(res.Rejected)
		q.state.LastSuccess = now
		q.mu.Unlock()
		q.opts.Observer.OutputSent(q.opts.Name, sent)
		if res.Rejected > 0 {
			q.opts.Observer.OutputDropped(q.opts.Name, ClassRejected, res.Rejected)
			q.opts.Logger.Warn("samples rejected by the backend", "component", "output", "output", q.opts.Name,
				"rejected", res.Rejected, "message", res.Message)
		}
		return 0
	}
	se, ok := errors.AsType[*Error](err)
	if !ok {
		se = &Error{Class: ClassTransient, Err: err}
	}
	q.opts.Observer.OutputSendFailed(q.opts.Name, se.Class)
	q.mu.Lock()
	defer q.mu.Unlock()
	q.state.LastError, q.state.LastErrorAt = se.Error(), now
	if se.Class == ClassOutOfOrder {
		if len(q.spans) == 0 {
			q.splitSends = 1
		}
		if q.split(batch) {
			q.attempt = 0
			q.opts.Logger.Debug("batch refused for out of order samples: sending its halves", "component", "output",
				"output", q.opts.Name, "samples", len(batch), "requests", q.splitSends)
			return 0
		}
	}
	if !se.Retryable() {
		q.done(last)
		q.attempt = 0
		q.state.Dropped += int64(len(batch))
		q.opts.Observer.OutputDropped(q.opts.Name, se.Class, len(batch))
		q.opts.Logger.Warn("samples dropped: the backend rejected the batch", "component", "output", "output", q.opts.Name,
			"samples", len(batch), "class", se.Class, "status", se.Status, "error", se.Err)
		return 0
	}
	q.sending = 0
	q.attempt++
	delay := se.RetryAfter
	if delay <= 0 {
		ceiling := float64(initialBackoff) * math.Pow(2, float64(q.attempt-1))
		delay = time.Duration(q.opts.Rand.Float64() * min(ceiling, float64(maxBackoff)))
	}
	q.opts.Logger.Warn("sending failed, retrying", "component", "output", "output", q.opts.Name, "samples", len(batch),
		"class", se.Class, "status", se.Status, "error", se.Err, "attempt", q.attempt, "retry_in", delay.String())
	return max(delay, time.Millisecond)
}

// remove drops the queued points up to seq (some may have been dropped
// already); q.mu must be held.
func (q *Queue) remove(seq uint64) {
	n := 0
	for n < len(q.points) && q.points[n].seq <= seq {
		n++
	}
	q.points = append(q.points[:0], q.points[n:]...)
	q.sending = 0
	q.state.Queue = len(q.points)
	if len(q.points) == 0 {
		close(q.drained)
		q.drained = make(chan struct{})
	}
}

// Flush sends what is queued without waiting for the flush interval; it
// returns when the queue is empty, or with an error when ctx ends first.
func (q *Queue) Flush(ctx context.Context) error {
	q.mu.Lock()
	if len(q.points) == 0 {
		q.mu.Unlock()
		return nil
	}
	q.flushes++
	drained := q.drained
	q.mu.Unlock()
	q.signal()
	defer func() {
		q.mu.Lock()
		q.flushes--
		q.mu.Unlock()
	}()
	select {
	case <-drained:
		return nil
	case <-ctx.Done():
		q.mu.Lock()
		left := len(q.points)
		q.mu.Unlock()
		return fmt.Errorf("%d samples not sent: %w", left, ctx.Err())
	}
}
