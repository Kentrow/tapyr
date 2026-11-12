// Package collect executes job runs: it builds requests, applies auth,
// follows pagination and classifies errors (see
// docs/spec/05-collection.md). Every request goes through a budget
// reservation, retries and pagination pages included. Transient and
// rate-limited failures are retried within the run deadline, and outcomes
// are reported to the circuit breakers of the budget engine.
package collect

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"

	"github.com/kentrow/tapyr/internal/auth"
	"github.com/kentrow/tapyr/internal/budget"
	"github.com/kentrow/tapyr/internal/clock"
	"github.com/kentrow/tapyr/internal/config"
	"github.com/kentrow/tapyr/internal/mapping"
	"github.com/kentrow/tapyr/internal/runtime"
)

// Class is the error class of a failed request.
type Class string

// Error classes, see "Error classification" in 05-collection.md.
const (
	ClassRateLimited Class = "rate_limited"
	ClassAuth        Class = "auth"
	ClassTransient   Class = "transient"
	ClassClient      Class = "client"
	ClassDecode      Class = "decode"
	ClassBudget      Class = "budget"
)

// Error is a failed request.
type Error struct {
	Class    Class
	Endpoint string
	Status   int
	Err      error
}

func (e *Error) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("%s %s: HTTP %d: %v", e.Class, e.Endpoint, e.Status, e.Err)
	}
	return fmt.Sprintf("%s %s: %v", e.Class, e.Endpoint, e.Err)
}

func (e *Error) Unwrap() error { return e.Err }

// Options configures an Executor.
type Options struct {
	Client *http.Client
	// ClientFor returns the client of an account with TLS settings of its
	// own, or nil for Client; optional.
	ClientFor func(account string) *http.Client
	Budget    budget.Reserver
	Clock     clock.Clock
	Logger    *slog.Logger
	// MaxConcurrentRequests bounds in-flight requests across all jobs.
	MaxConcurrentRequests int
	// MaxBodyBytes bounds a response body. Default 64 MiB.
	MaxBodyBytes int64
	// Retry configures retries; MaxAttempts 0 or 1 disables them.
	Retry config.RetrySettings
	// Rand draws the backoff jitter; inject a seeded source in tests.
	Rand *rand.Rand
	// Observer receives the requests sent (self-metrics); optional.
	Observer Observer
}

// Observer receives the requests of an executor. endpoint is the templated
// path; status is 0 when no response was received, class is empty for a
// successful response and size is -1 when the body is not read here.
type Observer interface {
	Request(provider, account, job, endpoint string, status int, class string, d time.Duration, size int)
	Retry(provider, account, job, reason string)
	InFlight(provider, account string, delta int)
}

type nopObserver struct{}

func (nopObserver) Request(string, string, string, string, int, string, time.Duration, int) {}
func (nopObserver) Retry(string, string, string, string)                                    {}
func (nopObserver) InFlight(string, string, int)                                            {}

// Executor runs jobs. It is safe for concurrent use.
type Executor struct {
	client    *http.Client
	clientFor func(account string) *http.Client
	budget    budget.Reserver
	clock     clock.Clock
	logger    *slog.Logger
	sem       chan struct{}
	maxBody   int64
	retry     config.RetrySettings
	obs       Observer

	randMu sync.Mutex
	rand   *rand.Rand
}

const defaultMaxBodyBytes = 64 << 20

// New returns an Executor.
func New(opts Options) *Executor {
	e := &Executor{
		client: opts.Client, clientFor: opts.ClientFor, budget: opts.Budget, clock: opts.Clock, logger: opts.Logger,
		maxBody: opts.MaxBodyBytes, retry: opts.Retry, rand: opts.Rand, obs: opts.Observer,
	}
	if e.obs == nil {
		e.obs = nopObserver{}
	}
	if e.rand == nil {
		e.rand = rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64())) //nolint:gosec // G404: backoff jitter needs no cryptographic randomness.
	}
	if e.client == nil {
		e.client = &http.Client{}
	}
	if e.logger == nil {
		e.logger = slog.New(slog.DiscardHandler)
	}
	if e.maxBody <= 0 {
		e.maxBody = defaultMaxBodyBytes
	}
	if opts.MaxConcurrentRequests > 0 {
		e.sem = make(chan struct{}, opts.MaxConcurrentRequests)
	}
	return e
}

// clientOf returns the HTTP client of an account. Whoever built it, the
// client never follows redirects by itself: a redirect followed inside Do
// would be a request outside the budget engine, and Go keeps custom
// headers (credentials) on redirects to other hosts. fetch follows the
// redirects the provider allows, one reservation each.
func (e *Executor) clientOf(account string) *http.Client {
	c := e.client
	if e.clientFor != nil {
		if fc := e.clientFor(account); fc != nil {
			c = fc
		}
	}
	nc := *c
	nc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &nc
}

// maxRedirects bounds the redirects followed in a row by one request.
const maxRedirects = 5

// redirect is the outcome of an attempt answered by a redirect that may be
// followed: fetch sends the next request.
type redirect struct {
	target *url.URL
	method string
	body   []byte
}

func (r *redirect) Error() string { return "redirected to " + r.target.Host }

// redirectOf checks the redirect of a response to u: it may be followed
// within the origin of u, or to a host the provider allows
// (redirects.hosts), never from https to http. 307 and 308 keep the method
// and the body; the others become a GET without body.
func (r *run) redirectOf(u *url.URL, method string, body []byte, resp *http.Response) error {
	fail := func(format string, args ...any) error {
		return &Error{Class: ClassClient, Endpoint: r.job.Request.Path, Status: resp.StatusCode, Err: fmt.Errorf(format, args...)}
	}
	loc := resp.Header.Get("Location")
	if loc == "" {
		return fail("redirect without Location")
	}
	target, err := u.Parse(loc)
	if err != nil {
		return fail("invalid redirect Location: %w", sanitizeURLError(err))
	}
	if target.Scheme != u.Scheme && target.Scheme != "https" {
		return fail("refusing a redirect from %s to %s", u.Scheme, target.Scheme)
	}
	if target.Host != u.Host && !hostAllowed(target.Hostname(), r.job.RedirectHosts) {
		return fail("redirected to %s, which the provider does not allow (redirects.hosts)", target.Host)
	}
	next := &redirect{target: target, method: method, body: body}
	if resp.StatusCode != http.StatusTemporaryRedirect && resp.StatusCode != http.StatusPermanentRedirect && method != http.MethodHead {
		next.method, next.body = http.MethodGet, nil
	}
	return next
}

// hostAllowed reports whether host matches one of the patterns: a host
// name, or "*." and a domain for the hosts under it.
func hostAllowed(host string, patterns []string) bool {
	host = strings.ToLower(host)
	for _, p := range patterns {
		if domain, ok := strings.CutPrefix(p, "*."); ok {
			if strings.HasSuffix(host, "."+domain) {
				return true
			}
		} else if host == p {
			return true
		}
	}
	return false
}

// Result is what a run did: its pages, requests and retries.
type Result struct {
	Pages    []mapping.Page
	Requests int
	// Retries counts the retried requests.
	Retries int
	// Truncated reports that pagination stopped at maxPages.
	Truncated bool
}

// Run executes every request of one run of job. now is the run start time,
// available to CEL as now; deadline (zero for none) bounds waits and
// retries, on the injected clock. When the requests fail, the result still
// tells what the run did (requests, retries, the pages received before the
// failure), next to the error.
func (e *Executor) Run(ctx context.Context, job *runtime.Job, now, deadline time.Time) (*Result, error) {
	r := &run{ctx: ctx, e: e, job: job, now: now, deadline: deadline, res: &Result{}}
	r.input = &mapping.Input{Vars: job.Vars, Account: job.AccountInfo, Target: job.TargetInfo, Now: now}
	base, err := r.baseURL()
	if err != nil {
		return nil, err
	}
	query, err := r.paramValues(job.Request.Query)
	if err != nil {
		return nil, err
	}
	var body []byte
	if len(job.Request.Body) > 0 {
		values, err := r.paramValues(job.Request.Body)
		if err != nil {
			return nil, err
		}
		obj := make(map[string]string, len(values))
		for k := range values {
			obj[k] = values.Get(k)
		}
		if body, err = json.Marshal(obj); err != nil {
			return nil, err
		}
	}
	switch p := job.Pagination; p.Type {
	case config.PaginationOffset:
		err = r.offset(base, query, body)
	case config.PaginationPage:
		err = r.pageNumbers(base, query, body)
	case config.PaginationLinkHeader:
		err = r.linkHeader(base, query, body)
	case config.PaginationCursor:
		err = r.cursor(base, query, body)
	case config.PaginationExpression:
		err = r.expression(base, query, body)
	default:
		u := *base
		u.RawQuery = query.Encode()
		_, err = r.fetch(&u, body)
	}
	if err != nil {
		return r.res, err
	}
	return r.res, nil
}

type run struct {
	// cursorHeader is sent as the cursor header of the next request.
	cursorHeader string

	ctx      context.Context // scoped to one run
	e        *Executor
	job      *runtime.Job
	now      time.Time
	deadline time.Time
	input    *mapping.Input
	res      *Result
}

// baseURL resolves the path placeholders of the request. Values are
// path-escaped: "a/b" stays one segment.
func (r *run) baseURL() (*url.URL, error) {
	decoded, escaped := r.job.Request.Path, r.job.Request.Path
	for _, v := range config.PathVars(r.job.Request.Path) {
		value, ok := r.job.Vars[v]
		if !ok || value == "" {
			return nil, &Error{Class: ClassClient, Endpoint: r.job.Request.Path, Err: fmt.Errorf("path variable {%s} has no value", v)}
		}
		decoded = strings.ReplaceAll(decoded, "{"+v+"}", value)
		escaped = strings.ReplaceAll(escaped, "{"+v+"}", url.PathEscape(value))
	}
	u := *r.job.BaseURL
	u.Path = r.job.BaseURL.Path + decoded
	u.RawPath = r.job.BaseURL.EscapedPath() + escaped
	return &u, nil
}

func (r *run) paramValues(params []runtime.Param) (url.Values, error) {
	values := url.Values{}
	act := &mapping.Activation{Vars: r.job.Vars, Account: r.job.AccountInfo, Target: r.job.TargetInfo, Now: r.now}
	for _, p := range params {
		switch {
		case p.Expr != nil:
			v, err := p.Expr.Eval(act)
			if err == nil && v == types.NullValue {
				continue // null omits the parameter (an optional param)
			}
			var s string
			if err == nil {
				s, err = mapping.ToLabel(v)
			}
			if err != nil {
				return nil, &Error{Class: ClassClient, Endpoint: r.job.Request.Path, Err: fmt.Errorf("parameter %s: %w", p.Name, err)}
			}
			values.Set(p.Name, s)
		case !p.Secret.IsZero():
			values.Set(p.Name, p.Secret.Reveal())
		default:
			values.Set(p.Name, p.Literal)
		}
	}
	return values, nil
}

func (r *run) offset(base *url.URL, query url.Values, body []byte) error {
	p := r.job.Pagination
	o := p.Offset
	for offset, pages := o.Start, 0; ; offset, pages = offset+o.PageSize, pages+1 {
		if pages >= p.MaxPages {
			r.truncated()
			return nil
		}
		q := cloneValues(query)
		q.Set(o.OffsetParam, strconv.Itoa(offset))
		q.Set(o.LimitParam, strconv.Itoa(o.PageSize))
		u := *base
		u.RawQuery = q.Encode()
		page, err := r.fetch(&u, body)
		if err != nil {
			return err
		}
		items, err := r.job.Extract.Items(page, r.input)
		if err != nil {
			return &Error{Class: ClassDecode, Endpoint: r.job.Request.Path, Err: err}
		}
		done, err := r.listDone(page, offset+o.PageSize, o.PageSize, len(items))
		if err != nil || done {
			return err
		}
	}
}

// pageNumbers requests pages by page number.
func (r *run) pageNumbers(base *url.URL, query url.Values, body []byte) error {
	p := r.job.Pagination
	pp := p.Page
	first := 1
	if pp.Start != nil {
		first = *pp.Start
	}
	for number, pages := first, 0; ; number, pages = number+1, pages+1 {
		if pages >= p.MaxPages {
			r.truncated()
			return nil
		}
		q := cloneValues(query)
		q.Set(pp.PageParam, strconv.Itoa(number))
		if pp.SizeParam != "" && pp.PageSize > 0 {
			q.Set(pp.SizeParam, strconv.Itoa(pp.PageSize))
		}
		u := *base
		u.RawQuery = q.Encode()
		page, err := r.fetch(&u, body)
		if err != nil {
			return err
		}
		items, err := r.job.Extract.Items(page, r.input)
		if err != nil {
			return &Error{Class: ClassDecode, Endpoint: r.job.Request.Path, Err: err}
		}
		done, err := r.listDone(page, (pages+1)*pp.PageSize, pp.PageSize, len(items))
		if err != nil || done {
			return err
		}
	}
}

// cursor passes the cursor extracted from each response to the next request.
func (r *run) cursor(base *url.URL, query url.Values, body []byte) error {
	p := r.job.Pagination
	c := p.Cursor
	q := cloneValues(query)
	if c.PageSizeParam != "" && c.PageSize > 0 {
		q.Set(c.PageSizeParam, strconv.Itoa(c.PageSize))
	}
	next := ""
	for pages := 0; ; pages++ {
		if pages >= p.MaxPages {
			r.truncated()
			return nil
		}
		pq := cloneValues(q)
		if next != "" && c.Param != "" {
			pq.Set(c.Param, next)
		}
		u := *base
		u.RawQuery = pq.Encode()
		r.cursorHeader = ""
		if next != "" && c.Header != "" {
			r.cursorHeader = next
		}
		page, err := r.fetch(&u, body)
		if err != nil {
			return err
		}
		v, err := p.Next.Eval(r.pageActivation(page))
		if err == nil {
			next, err = mapping.ToLabel(v)
		}
		if err != nil {
			return &Error{Class: ClassDecode, Endpoint: r.job.Request.Path, Err: fmt.Errorf("pagination cursor: %w", err)}
		}
		if next == "" {
			return nil
		}
	}
}

// expression computes the query of the next request with CEL; null stops.
func (r *run) expression(base *url.URL, query url.Values, body []byte) error {
	p := r.job.Pagination
	q := cloneValues(query)
	for pages := 0; ; pages++ {
		if pages >= p.MaxPages {
			r.truncated()
			return nil
		}
		u := *base
		u.RawQuery = q.Encode()
		page, err := r.fetch(&u, body)
		if err != nil {
			return err
		}
		v, err := p.Next.Eval(r.pageActivation(page))
		if err != nil {
			return &Error{Class: ClassDecode, Endpoint: r.job.Request.Path, Err: fmt.Errorf("pagination next: %w", err)}
		}
		if v.Type() == types.NullType {
			return nil
		}
		params, err := nextQuery(v)
		if err != nil {
			return &Error{Class: ClassDecode, Endpoint: r.job.Request.Path, Err: err}
		}
		for k, val := range params {
			q.Set(k, val)
		}
	}
}

// nextQuery reads the {query: {...}} result of an expression pagination.
func nextQuery(v ref.Val) (map[string]string, error) {
	errShape := errors.New("pagination next must return {query: {...}} or null")
	m, ok := asStringMap(v)
	if !ok {
		return nil, errShape
	}
	raw, ok := m["query"]
	if !ok {
		return nil, errShape
	}
	query, ok := asStringMap(types.DefaultTypeAdapter.NativeToValue(raw))
	if !ok {
		return nil, errShape
	}
	out := make(map[string]string, len(query))
	for k, val := range query {
		s, err := mapping.ToLabel(types.DefaultTypeAdapter.NativeToValue(val))
		if err != nil {
			return nil, fmt.Errorf("pagination next: query %s: %w", k, err)
		}
		out[k] = s
	}
	return out, nil
}

// listDone evaluates the stop conditions of offset and page pagination: stop
// is true, the items requested so far reach total, or the page is not full
// (empty when the page size is unknown).
func (r *run) listDone(page *mapping.Page, requested, pageSize, items int) (bool, error) {
	p := r.job.Pagination
	act := r.pageActivation(page)
	if p.Stop != nil {
		v, err := p.Stop.Eval(act)
		var stop bool
		if err == nil {
			stop, err = mapping.ToBool(v)
		}
		if err != nil {
			return false, &Error{Class: ClassDecode, Endpoint: r.job.Request.Path, Err: fmt.Errorf("pagination stop: %w", err)}
		}
		if stop {
			return true, nil
		}
	}
	if p.Total != nil {
		v, err := p.Total.Eval(act)
		var total float64
		if err == nil {
			total, err = mapping.ToFloat(v)
		}
		if err != nil {
			return false, &Error{Class: ClassDecode, Endpoint: r.job.Request.Path, Err: fmt.Errorf("pagination total: %w", err)}
		}
		if float64(requested) >= total {
			return true, nil
		}
	}
	if pageSize <= 0 {
		return items == 0, nil
	}
	return items < pageSize, nil
}

func (r *run) pageActivation(page *mapping.Page) *mapping.Activation {
	return &mapping.Activation{
		Response: map[string]any{"status": int64(page.Status), "headers": headersValue(page.Headers), "body": page.Body},
		Vars:     r.job.Vars, Account: r.job.AccountInfo, Target: r.job.TargetInfo, Now: r.now,
	}
}

func (r *run) linkHeader(base *url.URL, query url.Values, body []byte) error {
	p := r.job.Pagination
	lh := p.LinkHeader
	q := cloneValues(query)
	if lh != nil && lh.PageSizeParam != "" && lh.PageSize > 0 {
		q.Set(lh.PageSizeParam, strconv.Itoa(lh.PageSize))
	}
	rel := "next"
	if lh != nil && lh.Rel != "" {
		rel = lh.Rel
	}
	next := *base
	next.RawQuery = q.Encode()
	u := &next
	for pages := 0; ; pages++ {
		if pages >= p.MaxPages {
			r.truncated()
			return nil
		}
		page, err := r.fetch(u, body)
		if err != nil {
			return err
		}
		link := findLink(page.Headers.Values("Link"), rel)
		if link == "" {
			return nil
		}
		target, err := u.Parse(link)
		if err != nil {
			return &Error{Class: ClassDecode, Endpoint: r.job.Request.Path, Err: fmt.Errorf("invalid Link URL %q: %w", link, err)}
		}
		if target.Scheme != base.Scheme || target.Host != base.Host {
			return &Error{
				Class: ClassDecode, Endpoint: r.job.Request.Path,
				Err: fmt.Errorf("refusing to follow a Link to another origin (%s://%s)", target.Scheme, target.Host),
			}
		}
		u = target
	}
}

func (r *run) truncated() {
	r.res.Truncated = true
	r.e.logger.Warn("pagination stopped at maxPages", "component", "collect", "provider", r.job.Provider,
		"account", r.job.Account, "job", r.job.Name, "endpoint", r.job.Request.Path, "max_pages", r.job.Pagination.MaxPages)
}

// fetch sends a request, retrying transient and rate-limited failures
// within the run deadline (see "Retry policy" in 05-collection.md).
func (r *run) fetch(u *url.URL, body []byte) (*mapping.Page, error) {
	req := r.job.Request
	retryable := req.Method != http.MethodPost || req.Idempotent
	authRetried := false
	method, redirects := req.Method, 0
	for attempt := 0; ; attempt++ {
		page, err := r.attempt(u, method, body, attempt > 0)
		if next, ok := errors.AsType[*redirect](err); ok {
			if redirects++; redirects > maxRedirects {
				return nil, &Error{Class: ClassClient, Endpoint: req.Path, Err: fmt.Errorf("more than %d redirects", maxRedirects)}
			}
			// A redirect is a new request, not a retry.
			u, method, body, attempt = next.target, next.method, next.body, -1
			continue
		}
		ce, ok := errors.AsType[*Error](err)
		if err == nil || !ok {
			return page, err
		}
		switch ce.Class {
		case ClassAuth:
			if authRetried || r.job.Auth == nil || ce.Status == 0 {
				return nil, err
			}
			retry, aerr := r.job.Auth.OnAuthFailure(r.ctx, &http.Response{StatusCode: ce.Status})
			if aerr != nil || !retry {
				return nil, err
			}
			authRetried = true
		case ClassTransient, ClassRateLimited:
			if !retryable || attempt+1 >= r.e.retry.MaxAttempts {
				return nil, err
			}
			if ce.Class == ClassTransient {
				// Rate-limited retries need no backoff: the budget engine
				// makes the next reservation wait for the penalty.
				wait := r.e.backoff(attempt)
				if !r.deadline.IsZero() && r.e.clock.Now().Add(wait).After(r.deadline) {
					return nil, err
				}
				if r.e.clock.Sleep(r.ctx, wait) != nil {
					return nil, err
				}
			}
		default:
			return nil, err
		}
		r.res.Retries++
		r.e.obs.Retry(r.job.Provider, r.job.Account, r.job.Name, string(ce.Class))
		r.e.logger.Debug("retrying", "component", "collect", "provider", r.job.Provider, "account", r.job.Account,
			"job", r.job.Name, "endpoint", req.Path, "class", string(ce.Class), "attempt", attempt+2)
	}
}

// attempt sends one request through the budget engine and decodes the
// response.
func (r *run) attempt(u *url.URL, method string, body []byte, retry bool) (*mapping.Page, error) {
	job := r.job
	outcome := budget.Outcome{}
	var observed Class
	fail := func(class Class, status int, err error) error {
		observed = class
		outcome.Transient = class == ClassTransient
		return &Error{Class: class, Endpoint: job.Request.Path, Status: status, Err: err}
	}
	ctx := r.ctx
	var reader io.Reader = http.NoBody
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), reader)
	if err != nil {
		return nil, &Error{Class: ClassClient, Endpoint: job.Request.Path, Err: err}
	}
	for k, v := range job.Request.Headers {
		req.Header.Set(k, v)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if r.cursorHeader != "" && job.Pagination.Cursor != nil {
		req.Header.Set(job.Pagination.Cursor.Header, r.cursorHeader)
	}
	// Credentials first: a token request reserves budget of its own, which
	// must not wait while this request holds a reservation.
	if job.Auth != nil {
		if err := job.Auth.Decorate(ctx, req); err != nil {
			class := ClassAuth
			if _, refused := budget.IsRefusal(err); refused {
				class = ClassBudget
			}
			return nil, &Error{Class: class, Endpoint: job.Request.Path, Err: err}
		}
	}
	res, err := r.e.budget.Reserve(ctx, budget.Descriptor{
		Provider: job.Provider, Account: job.Account, Target: job.Target, Credential: job.Credential,
		Endpoint: job.Request.Path, Method: method,
		Vars: job.Vars, Job: job.Name, Priority: job.Priority, Retry: retry, Deadline: r.deadline,
	})
	if err != nil {
		return nil, &Error{Class: ClassBudget, Endpoint: job.Request.Path, Err: err}
	}
	defer func() { res.Done(outcome) }()

	if r.e.sem != nil {
		select {
		case r.e.sem <- struct{}{}:
			defer func() { <-r.e.sem }()
		case <-ctx.Done():
			outcome.Err = ctx.Err()
			return nil, fail(ClassTransient, 0, ctx.Err())
		}
	}

	reqCtx, cancel := context.WithTimeout(ctx, job.Request.Timeout)
	defer cancel()
	req = req.WithContext(reqCtx)

	started := r.e.clock.Now()
	r.e.obs.InFlight(job.Provider, job.Account, 1)
	var received time.Time
	size := 0
	defer func() {
		if received.IsZero() {
			received = r.e.clock.Now()
		}
		r.e.obs.InFlight(job.Provider, job.Account, -1)
		r.e.obs.Request(job.Provider, job.Account, job.Name, job.Request.Path, outcome.Status, string(observed),
			received.Sub(started), size)
	}()
	resp, err := r.e.clientOf(r.job.Account).Do(req)
	r.res.Requests++
	if err != nil {
		outcome.Err = err
		return nil, fail(ClassTransient, 0, sanitizeURLError(err))
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, r.e.maxBody+1))
	received, size = r.e.clock.Now(), len(data)
	outcome.Status, outcome.Header = resp.StatusCode, resp.Header
	r.e.logger.Debug("request", "component", "collect", "provider", job.Provider, "account", job.Account,
		"job", job.Name, "endpoint", job.Request.Path, "status", resp.StatusCode,
		"duration_ms", received.Sub(started).Milliseconds())
	if err != nil {
		outcome.Err = err
		return nil, fail(ClassTransient, resp.StatusCode, fmt.Errorf("reading the response: %w", err))
	}
	if int64(len(data)) > r.e.maxBody {
		return nil, fail(ClassDecode, resp.StatusCode, fmt.Errorf("response larger than %d bytes", r.e.maxBody))
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 && resp.StatusCode != http.StatusNotModified {
		return nil, r.redirectOf(u, method, body, resp)
	}
	if class := classify(job.Errors, resp.StatusCode); class != "" {
		return nil, fail(class, resp.StatusCode, fmt.Errorf("response: %s", excerpt(data)))
	}
	page := &mapping.Page{Status: resp.StatusCode, Headers: resp.Header, ReceivedAt: received}
	if len(bytes.TrimSpace(data)) > 0 {
		if page.Body, err = mapping.DecodeJSON(data); err != nil {
			return nil, fail(ClassDecode, resp.StatusCode, err)
		}
	}
	r.res.Pages = append(r.res.Pages, *page)
	return &r.res.Pages[len(r.res.Pages)-1], nil
}

// authRequestTimeout bounds token and login requests.
const authRequestTimeout = 30 * time.Second

// SendAuth implements auth.Sender: token and login requests reserve budget
// as auth requests (critical, so that they are never starved by the jobs
// waiting for them) and count in the concurrency cap. The caller reads and
// closes the response body.
func (e *Executor) SendAuth(ctx context.Context, scope auth.Scope, endpoint string, req *http.Request) (*http.Response, error) {
	res, err := e.budget.Reserve(ctx, budget.Descriptor{
		Provider: scope.Provider, Account: scope.Account, Target: scope.Target, Credential: scope.Credential,
		Endpoint: endpoint, Method: req.Method, Priority: config.PriorityCritical, AuthRequest: true,
	})
	if err != nil {
		return nil, err
	}
	outcome := budget.Outcome{}
	defer func() { res.Done(outcome) }()
	if e.sem != nil {
		select {
		case e.sem <- struct{}{}:
			defer func() { <-e.sem }()
		case <-ctx.Done():
			outcome.Err = ctx.Err()
			return nil, ctx.Err()
		}
	}
	reqCtx, cancel := context.WithTimeout(ctx, authRequestTimeout)
	started := e.clock.Now()
	e.obs.InFlight(scope.Provider, scope.Account, 1)
	defer e.obs.InFlight(scope.Provider, scope.Account, -1)
	// The URL comes from the provider definition and the account params.
	resp, err := e.clientOf(scope.Account).Do(req.WithContext(reqCtx))
	if err != nil {
		cancel()
		outcome.Err, outcome.Transient = err, true
		e.obs.Request(scope.Provider, scope.Account, "", endpoint, 0, string(ClassTransient), e.clock.Since(started), 0)
		return nil, sanitizeURLError(err)
	}
	outcome.Status, outcome.Header = resp.StatusCode, resp.Header
	// The body is read by the authenticator: its size is unknown (-1).
	e.obs.Request(scope.Provider, scope.Account, "", endpoint, resp.StatusCode, string(statusClass(resp.StatusCode)),
		e.clock.Since(started), -1)
	resp.Body = cancelOnClose{ReadCloser: resp.Body, cancel: cancel}
	e.logger.Debug("auth request", "component", "collect", "provider", scope.Provider, "account", scope.Account,
		"endpoint", endpoint, "status", resp.StatusCode)
	return resp, nil
}

// cancelOnClose releases the request context with the body.
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c cancelOnClose) Close() error {
	defer c.cancel()
	return c.ReadCloser.Close()
}

// backoff returns the delay before retry attempt+1: full jitter,
// random(0, min(maxBackoff, initialBackoff x multiplier^attempt)).
func (e *Executor) backoff(attempt int) time.Duration {
	ceiling := float64(e.retry.InitialBackoff) * math.Pow(e.retry.Multiplier, float64(attempt))
	if limit := float64(e.retry.MaxBackoff); limit > 0 && ceiling > limit {
		ceiling = limit
	}
	e.randMu.Lock()
	defer e.randMu.Unlock()
	return time.Duration(e.rand.Float64() * ceiling)
}

// statusClass classifies the status of a token or login request, which the
// authenticators interpret: "" below 400.
func statusClass(status int) Class {
	switch {
	case status < 400:
		return ""
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return ClassAuth
	case status == http.StatusTooManyRequests:
		return ClassRateLimited
	case status >= 500:
		return ClassTransient
	}
	return ClassClient
}

// classify returns the error class of a status, or "" for success.
func classify(e *config.ErrorSemantics, status int) Class {
	switch {
	case e != nil && e.RateLimited != nil && slices.Contains(e.RateLimited.Statuses, status):
		return ClassRateLimited
	case e != nil && e.AuthFailed != nil && slices.Contains(e.AuthFailed.Statuses, status):
		return ClassAuth
	case e != nil && e.Retryable != nil && slices.Contains(e.Retryable.Statuses, status):
		return ClassTransient
	case status >= 200 && status < 300:
		return ""
	}
	// Other non-success statuses are not retryable.
	return ClassClient
}

// excerpt returns the start of a body for error messages, on one line.
func excerpt(data []byte) string {
	const maxExcerpt = 200
	s := strings.Join(strings.Fields(string(data)), " ")
	if len(s) > maxExcerpt {
		s = s[:maxExcerpt] + "..."
	}
	if s == "" {
		return "(empty body)"
	}
	return s
}

// sanitizeURLError drops the URL from client errors: it may carry query
// parameters with secrets.
func sanitizeURLError(err error) error {
	if ue, ok := errors.AsType[*url.Error](err); ok {
		return fmt.Errorf("%s: %w", strings.ToLower(ue.Op), ue.Err)
	}
	return err
}

// findLink returns the target of the first link with the relation rel
// (RFC 8288).
func findLink(headers []string, rel string) string {
	for _, h := range headers {
		for part := range strings.SplitSeq(h, ",") {
			segs := strings.Split(part, ";")
			target := strings.TrimSpace(segs[0])
			if !strings.HasPrefix(target, "<") || !strings.HasSuffix(target, ">") {
				continue
			}
			for _, p := range segs[1:] {
				k, v, ok := strings.Cut(strings.TrimSpace(p), "=")
				if !ok || !strings.EqualFold(k, "rel") {
					continue
				}
				if slices.Contains(strings.Fields(strings.Trim(v, `"`)), rel) {
					return target[1 : len(target)-1]
				}
			}
		}
	}
	return ""
}

func headersValue(h http.Header) map[string]any {
	out := make(map[string]any, len(h))
	for k, vs := range h {
		list := make([]any, len(vs))
		for i, v := range vs {
			list[i] = v
		}
		out[strings.ToLower(k)] = list
	}
	return out
}

func cloneValues(v url.Values) url.Values {
	out := make(url.Values, len(v))
	for k, vs := range v {
		out[k] = slices.Clone(vs)
	}
	return out
}

// asStringMap converts a CEL map value to a Go map with string keys.
func asStringMap(v ref.Val) (map[string]any, bool) {
	native, err := v.ConvertToNative(reflect.TypeFor[map[string]any]())
	if err != nil {
		return nil, false
	}
	m, ok := native.(map[string]any)
	return m, ok
}
