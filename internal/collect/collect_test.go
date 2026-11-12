package collect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kentrow/tapyr/internal/auth"
	"github.com/kentrow/tapyr/internal/budget"
	"github.com/kentrow/tapyr/internal/budget/budgettest"
	"github.com/kentrow/tapyr/internal/clock"
	"github.com/kentrow/tapyr/internal/config"
	"github.com/kentrow/tapyr/internal/mapping"
	"github.com/kentrow/tapyr/internal/runtime"
	"github.com/kentrow/tapyr/internal/secret"
)

var start = time.Date(2026, time.November, 2, 12, 0, 0, 0, time.UTC)

// recorder is a budget.Reserver that records every reservation.
type recorder struct {
	mu       sync.Mutex
	reserved []budget.Descriptor
	outcomes []budget.Outcome
	refuse   error
}

func (r *recorder) Reserve(_ context.Context, d budget.Descriptor) (budget.Reservation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.refuse != nil {
		return nil, r.refuse
	}
	r.reserved = append(r.reserved, d)
	return reservation{r}, nil
}

type reservation struct{ r *recorder }

func (res reservation) Done(o budget.Outcome) {
	res.r.mu.Lock()
	defer res.r.mu.Unlock()
	res.r.outcomes = append(res.r.outcomes, o)
}

func newJob(t *testing.T, base string, extract *config.Extract) *runtime.Job {
	t.Helper()
	env, err := mapping.NewEnv(0)
	if err != nil {
		t.Fatal(err)
	}
	x, errs := env.CompileExtract(extract)
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	return &runtime.Job{
		Name: "devices", Account: "acme", Provider: "example", BaseURL: u,
		Request:    runtime.Request{Method: http.MethodGet, Path: "/devices", Headers: map[string]string{}, Timeout: 5 * time.Second},
		Pagination: runtime.Pagination{Type: config.PaginationNone, MaxPages: 100},
		Extract:    x,
		Errors: &config.ErrorSemantics{
			RateLimited: &config.RateLimitedErrors{Statuses: []int{429}},
			AuthFailed:  &config.StatusMatcher{Statuses: []int{401}},
			Retryable:   &config.StatusMatcher{Statuses: []int{500, 502, 503, 504}},
		},
		Vars:        map[string]string{},
		AccountInfo: map[string]any{"name": "acme"},
	}
}

func newExecutor(rec budget.Reserver) *Executor {
	return New(Options{Budget: rec, Clock: clock.NewFake(start), MaxConcurrentRequests: 4})
}

func compile(t *testing.T, ctx mapping.Context, src string, out mapping.Output) *mapping.Program {
	t.Helper()
	env, err := mapping.NewEnv(0)
	if err != nil {
		t.Fatal(err)
	}
	p, err := env.Compile(ctx, src, out)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// items serves a JSON list of n items, paginated by offset/limit.
func offsetServer(t *testing.T, n int, seen *[]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen = append(*seen, r.URL.RawQuery)
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		items := []map[string]any{}
		for i := offset; i < n && i < offset+limit; i++ {
			items = append(items, map[string]any{"id": i})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"total": n, "items": items})
	}))
}

func TestRunSingleRequest(t *testing.T) {
	var got *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		w.Header().Set("X-Total", "2")
		fmt.Fprint(w, `[{"id": 1}, {"id": 2}]`)
	}))
	defer srv.Close()

	job := newJob(t, srv.URL+"/api/v1", nil)
	job.Request.Path = "/sites/{siteId}/devices"
	job.Vars = map[string]string{"siteId": "north 01/b"}
	job.Request.Headers = map[string]string{"Accept": "application/json"}
	job.Request.Query = []runtime.Param{
		{Name: "verbose", Literal: "true"},
		{Name: "key", Secret: secret.New("s3cr3t")},
		{Name: "since", Expr: compile(t, mapping.ContextRequest, "string(int(now) - 300)", mapping.OutputString)},
		// null omits the parameter (an optional account param, for example).
		{Name: "aid", Expr: compile(t, mapping.ContextRequest, "dyn(null)", mapping.OutputString)},
		{Name: "empty", Expr: compile(t, mapping.ContextRequest, "''", mapping.OutputString)},
	}
	a, err := auth.New(&config.Auth{Type: config.AuthBearer, Bearer: &config.BearerAuth{Token: config.Value{Secret: "k"}}},
		auth.Secrets{"k": secret.New("tok")})
	if err != nil {
		t.Fatal(err)
	}
	job.Auth = a

	rec := &recorder{}
	res, err := newExecutor(rec).Run(context.Background(), job, start, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Requests != 1 || len(res.Pages) != 1 || res.Truncated {
		t.Fatalf("result = %+v", res)
	}
	if got.URL.EscapedPath() != "/api/v1/sites/north%2001%2Fb/devices" {
		t.Errorf("path = %s", got.URL.EscapedPath())
	}
	q := got.URL.Query()
	if q.Get("verbose") != "true" || q.Get("key") != "s3cr3t" || q.Get("since") != strconv.FormatInt(start.Unix()-300, 10) {
		t.Errorf("query = %v", q)
	}
	if _, sent := q["aid"]; sent {
		t.Errorf("a null value was sent: %v", q)
	}
	if v, sent := q["empty"]; !sent || v[0] != "" {
		t.Errorf("an empty value was not sent as such: %v", q)
	}
	if got.Header.Get("Authorization") != "Bearer tok" || got.Header.Get("Accept") != "application/json" {
		t.Errorf("headers = %v", got.Header)
	}
	page := res.Pages[0]
	if page.Status != 200 || page.Headers.Get("X-Total") != "2" || !page.ReceivedAt.Equal(start) {
		t.Errorf("page = %+v", page)
	}
	if diff := cmp.Diff([]any{map[string]any{"id": int64(1)}, map[string]any{"id": int64(2)}}, page.Body); diff != "" {
		t.Errorf("body: %s", diff)
	}
	// The budget engine sees the templated endpoint, never the resolved URL.
	if len(rec.reserved) != 1 || rec.reserved[0].Endpoint != "/sites/{siteId}/devices" || rec.reserved[0].Job != "devices" {
		t.Errorf("reservations = %+v", rec.reserved)
	}
	if len(rec.outcomes) != 1 || rec.outcomes[0].Status != 200 {
		t.Errorf("outcomes = %+v", rec.outcomes)
	}
}

func TestRunPostBody(t *testing.T) {
	var body map[string]string
	var contentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contentType = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&body)
		fmt.Fprint(w, `[]`)
	}))
	defer srv.Close()
	job := newJob(t, srv.URL, nil)
	job.Request.Method = http.MethodPost
	job.Request.Body = []runtime.Param{{Name: "query", Literal: "{ devices { id } }"}}
	if _, err := newExecutor(budgettest.Passthrough{}).Run(context.Background(), job, start, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if contentType != "application/json" || body["query"] != "{ devices { id } }" {
		t.Errorf("content type %q, body %v", contentType, body)
	}
}

func TestRunOffsetPagination(t *testing.T) {
	tests := []struct {
		name      string
		items     int
		stop      string
		total     string
		maxPages  int
		wantQuery []string
		truncated bool
	}{
		{
			name: "stops on a short page", items: 5,
			wantQuery: []string{"limit=2&offset=0", "limit=2&offset=2", "limit=2&offset=4"},
		},
		{
			name: "full last page needs one more request", items: 4,
			wantQuery: []string{"limit=2&offset=0", "limit=2&offset=2", "limit=2&offset=4"},
		},
		{
			name: "total", items: 4, total: "response.body.total",
			wantQuery: []string{"limit=2&offset=0", "limit=2&offset=2"},
		},
		{
			name: "stop expression", items: 10, stop: "response.body.items[0].id >= 2",
			wantQuery: []string{"limit=2&offset=0", "limit=2&offset=2"},
		},
		{
			name: "maxPages", items: 10, maxPages: 2, truncated: true,
			wantQuery: []string{"limit=2&offset=0", "limit=2&offset=2"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var seen []string
			srv := offsetServer(t, tt.items, &seen)
			defer srv.Close()
			job := newJob(t, srv.URL, &config.Extract{Items: "response.body.items"})
			job.Pagination = runtime.Pagination{
				Type: config.PaginationOffset, MaxPages: 100,
				Offset: &config.OffsetPagination{OffsetParam: "offset", LimitParam: "limit", PageSize: 2},
			}
			if tt.maxPages > 0 {
				job.Pagination.MaxPages = tt.maxPages
			}
			if tt.stop != "" {
				job.Pagination.Stop = compile(t, mapping.ContextPage, tt.stop, mapping.OutputBool)
			}
			if tt.total != "" {
				job.Pagination.Total = compile(t, mapping.ContextPage, tt.total, mapping.OutputNumber)
			}
			rec := &recorder{}
			res, err := newExecutor(rec).Run(context.Background(), job, start, time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tt.wantQuery, seen); diff != "" {
				t.Errorf("requests (-want +got):\n%s", diff)
			}
			if res.Requests != len(tt.wantQuery) || len(rec.reserved) != len(tt.wantQuery) || res.Truncated != tt.truncated {
				t.Errorf("requests %d, reservations %d, truncated %v", res.Requests, len(rec.reserved), res.Truncated)
			}
		})
	}
}

func TestRunLinkHeaderPagination(t *testing.T) {
	var seen []string
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.RequestURI())
		switch r.URL.Query().Get("page") {
		case "":
			w.Header().Add("Link", `<`+srv.URL+`/devices?perPage=2&page=2>; rel="next", <`+srv.URL+`/devices?page=9>; rel=last`)
		case "2":
			// Relative reference, resolved against the current URL.
			w.Header().Add("Link", `</devices?perPage=2&page=3>; rel="prev next"`)
		}
		fmt.Fprint(w, `[{"id": 1}]`)
	}))
	defer srv.Close()
	job := newJob(t, srv.URL, nil)
	job.Pagination = runtime.Pagination{
		Type: config.PaginationLinkHeader, MaxPages: 100,
		LinkHeader: &config.LinkHeaderPagination{PageSizeParam: "perPage", PageSize: 2, Rel: "next"},
	}
	res, err := newExecutor(budgettest.Passthrough{}).Run(context.Background(), job, start, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/devices?perPage=2", "/devices?perPage=2&page=2", "/devices?perPage=2&page=3"}
	if diff := cmp.Diff(want, seen); diff != "" {
		t.Errorf("requests (-want +got):\n%s", diff)
	}
	if len(res.Pages) != 3 {
		t.Errorf("pages = %d", len(res.Pages))
	}
}

func TestRunRefusesCrossOriginLinks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Link", `<https://evil.example.invalid/steal>; rel="next"`)
		fmt.Fprint(w, `[]`)
	}))
	defer srv.Close()
	job := newJob(t, srv.URL, nil)
	job.Pagination = runtime.Pagination{Type: config.PaginationLinkHeader, MaxPages: 100}
	_, err := newExecutor(budgettest.Passthrough{}).Run(context.Background(), job, start, time.Time{})
	if err == nil || !strings.Contains(err.Error(), "refusing to follow a Link to another origin (https://evil.example.invalid)") {
		t.Fatalf("error = %v", err)
	}
}

func TestRunErrors(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		body      string
		wantClass Class
		wantMsg   string
	}{
		{"rate limited", 429, `{"error":"slow down"}`, ClassRateLimited, `rate_limited /devices: HTTP 429: response: {"error":"slow down"}`},
		{"auth", 401, "", ClassAuth, "auth /devices: HTTP 401: response: (empty body)"},
		{"retryable", 503, "busy", ClassTransient, "HTTP 503"},
		{"client", 404, "not found", ClassClient, "client /devices: HTTP 404: response: not found"},
		{"unlisted server error", 501, "", ClassClient, "HTTP 501"},
		{"invalid JSON", 200, "{oops", ClassDecode, "decode /devices: HTTP 200: decoding JSON"},
		{"too large", 200, strings.Repeat("x", 2048), ClassDecode, "response larger than 1024 bytes"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				fmt.Fprint(w, tt.body)
			}))
			defer srv.Close()
			rec := &recorder{}
			e := New(Options{Budget: rec, Clock: clock.NewFake(start), MaxBodyBytes: 1024})
			_, err := e.Run(context.Background(), newJob(t, srv.URL, nil), start, time.Time{})
			ce, ok := errors.AsType[*Error](err)
			if !ok || ce.Class != tt.wantClass || !strings.Contains(err.Error(), tt.wantMsg) {
				t.Fatalf("error = %v (class %v), want %s containing %q", err, ce, tt.wantClass, tt.wantMsg)
			}
			if len(rec.outcomes) != 1 || rec.outcomes[0].Status != tt.status {
				t.Errorf("outcome = %+v", rec.outcomes)
			}
		})
	}
}

func TestRunTransportErrors(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		release := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}))
		defer srv.Close()
		defer close(release)
		job := newJob(t, srv.URL, nil)
		job.Request.Timeout = 50 * time.Millisecond
		_, err := newExecutor(budgettest.Passthrough{}).Run(context.Background(), job, start, time.Time{})
		if ce, ok := errors.AsType[*Error](err); !ok || ce.Class != ClassTransient || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("connection refused, URL not in the message", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		srv.Close()
		job := newJob(t, srv.URL, nil)
		job.Request.Query = []runtime.Param{{Name: "key", Secret: secret.New("s3cr3t")}}
		_, err := newExecutor(budgettest.Passthrough{}).Run(context.Background(), job, start, time.Time{})
		if ce, ok := errors.AsType[*Error](err); !ok || ce.Class != ClassTransient {
			t.Fatalf("error = %v", err)
		}
		if strings.Contains(err.Error(), "s3cr3t") || strings.Contains(err.Error(), srv.URL) {
			t.Errorf("error leaks the URL: %v", err)
		}
	})
	t.Run("budget refusal", func(t *testing.T) {
		rec := &recorder{refuse: errors.New("quota_exhausted")}
		_, err := newExecutor(rec).Run(context.Background(), newJob(t, "http://127.0.0.1:1", nil), start, time.Time{})
		if ce, ok := errors.AsType[*Error](err); !ok || ce.Class != ClassBudget {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("missing path variable", func(t *testing.T) {
		job := newJob(t, "http://127.0.0.1:1", nil)
		job.Request.Path = "/orgs/{orgId}"
		_, err := newExecutor(budgettest.Passthrough{}).Run(context.Background(), job, start, time.Time{})
		if err == nil || !strings.Contains(err.Error(), "path variable {orgId} has no value") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("canceled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := newExecutor(budgettest.Passthrough{}).Run(ctx, newJob(t, "http://127.0.0.1:1", nil), start, time.Time{})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestFindLink(t *testing.T) {
	tests := []struct {
		headers []string
		rel     string
		want    string
	}{
		{[]string{`<https://a/x?p=2>; rel="next"`}, "next", "https://a/x?p=2"},
		{[]string{`<https://a/1>; rel="first", <https://a/3>; rel=next`}, "next", "https://a/3"},
		{[]string{`<https://a/1>; rel="prev"`, `<https://a/3>; REL="next"`}, "next", "https://a/3"},
		{[]string{`<https://a/1>; rel="prev"`}, "next", ""},
		{[]string{`garbage`}, "next", ""},
	}
	for _, tt := range tests {
		if got := findLink(tt.headers, tt.rel); got != tt.want {
			t.Errorf("findLink(%q) = %q, want %q", tt.headers, got, tt.want)
		}
	}
}

// autoAdvance advances the fake clock whenever something sleeps on it,
// until the test ends.
func autoAdvance(t *testing.T, clk *clock.Fake, step time.Duration) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		for ctx.Err() == nil {
			if clk.BlockUntil(ctx, 1) == nil {
				clk.Advance(step)
			}
		}
	}()
}

func TestRetries(t *testing.T) {
	retry := config.RetrySettings{MaxAttempts: 3, InitialBackoff: config.Duration(time.Second), MaxBackoff: config.Duration(time.Minute), Multiplier: 2}
	tests := []struct {
		name         string
		statuses     []int
		method       string
		idempotent   bool
		deadline     time.Duration
		wantErr      bool
		wantRequests int
		wantRetries  []bool
	}{
		{name: "transient then success", statuses: []int{503, 200}, wantRequests: 2, wantRetries: []bool{false, true}},
		{name: "rate limited then success", statuses: []int{429, 200}, wantRequests: 2, wantRetries: []bool{false, true}},
		{name: "max attempts", statuses: []int{503, 503, 503, 200}, wantErr: true, wantRequests: 3, wantRetries: []bool{false, true, true}},
		{name: "client errors are not retried", statuses: []int{404}, wantErr: true, wantRequests: 1, wantRetries: []bool{false}},
		{name: "static auth failures are not retried", statuses: []int{401}, wantErr: true, wantRequests: 1, wantRetries: []bool{false}},
		{name: "non-idempotent POST", statuses: []int{503, 200}, method: http.MethodPost, wantErr: true, wantRequests: 1, wantRetries: []bool{false}},
		{name: "idempotent POST", statuses: []int{503, 200}, method: http.MethodPost, idempotent: true, wantRequests: 2, wantRetries: []bool{false, true}},
		{name: "backoff beyond the deadline", statuses: []int{503, 200}, deadline: time.Nanosecond, wantErr: true, wantRequests: 1, wantRetries: []bool{false}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var mu sync.Mutex
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				mu.Lock()
				status := tt.statuses[min(calls, len(tt.statuses)-1)]
				calls++
				mu.Unlock()
				w.WriteHeader(status)
				fmt.Fprint(w, `[]`)
			}))
			defer srv.Close()
			clk := clock.NewFake(start)
			autoAdvance(t, clk, time.Minute)
			rec := &recorder{}
			e := New(Options{Budget: rec, Clock: clk, Retry: retry, Rand: rand.New(rand.NewPCG(1, 2))})
			job := newJob(t, srv.URL, nil)
			if tt.method != "" {
				job.Request.Method = tt.method
				job.Request.Idempotent = tt.idempotent
			}
			var deadline time.Time
			if tt.deadline > 0 {
				deadline = start.Add(tt.deadline)
			}
			res, err := e.Run(context.Background(), job, start, deadline)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Run() error = %v, wantErr %v", err, tt.wantErr)
			}
			if calls != tt.wantRequests {
				t.Errorf("requests = %d, want %d", calls, tt.wantRequests)
			}
			var retries []bool
			for _, d := range rec.reserved {
				retries = append(retries, d.Retry)
				if !d.Deadline.Equal(deadline) {
					t.Errorf("descriptor deadline = %v, want %v", d.Deadline, deadline)
				}
			}
			if diff := cmp.Diff(tt.wantRetries, retries); diff != "" {
				t.Errorf("retry flags (-want +got):\n%s", diff)
			}
			if err == nil && res.Retries != tt.wantRequests-1 {
				t.Errorf("Retries = %d", res.Retries)
			}
			// Transient failures are reported to the breaker.
			for i, o := range rec.outcomes {
				if want := o.Status == 503; o.Transient != want {
					t.Errorf("outcome %d: transient %v for status %d", i, o.Transient, o.Status)
				}
			}
		})
	}
}

func TestBackoff(t *testing.T) {
	e := New(Options{Retry: config.RetrySettings{
		InitialBackoff: config.Duration(time.Second), MaxBackoff: config.Duration(10 * time.Second), Multiplier: 2,
	}, Rand: rand.New(rand.NewPCG(3, 4))})
	for attempt, ceiling := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 10 * time.Second, 10 * time.Second} {
		for range 50 {
			if d := e.backoff(attempt); d < 0 || d > ceiling {
				t.Fatalf("backoff(%d) = %v, want within [0, %v]", attempt, d, ceiling)
			}
		}
	}
}

func TestRunPagePagination(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.RawQuery)
		page, _ := strconv.Atoi(r.URL.Query().Get("p"))
		items := []int{}
		for i := (page - 1) * 2; i < 5 && i < page*2; i++ {
			items = append(items, i)
		}
		_ = json.NewEncoder(w).Encode(items)
	}))
	defer srv.Close()
	job := newJob(t, srv.URL, nil)
	start := 1
	job.Pagination = runtime.Pagination{
		Type: config.PaginationPage, MaxPages: 100,
		Page: &config.PagePagination{PageParam: "p", SizeParam: "size", PageSize: 2, Start: &start},
	}
	if _, err := newExecutor(budgettest.Passthrough{}).Run(context.Background(), job, start0(), time.Time{}); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]string{"p=1&size=2", "p=2&size=2", "p=3&size=2"}, seen); diff != "" {
		t.Errorf("requests (-want +got):\n%s", diff)
	}
}

func TestRunCursorPagination(t *testing.T) {
	for _, viaHeader := range []bool{false, true} {
		var seen []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			cursor := r.URL.Query().Get("after")
			if viaHeader {
				cursor = r.Header.Get("X-Cursor")
			}
			seen = append(seen, cursor)
			next := map[string]string{"": "c1", "c1": "c2", "c2": ""}[cursor]
			fmt.Fprintf(w, `{"next": %q, "items": [1]}`, next)
		}))
		job := newJob(t, srv.URL, &config.Extract{Items: "response.body.items"})
		c := &config.CursorPagination{Cursor: "response.body.next", Param: "after", PageSizeParam: "n", PageSize: 1}
		if viaHeader {
			c.Param, c.Header = "", "X-Cursor"
		}
		job.Pagination = runtime.Pagination{
			Type: config.PaginationCursor, MaxPages: 100, Cursor: c,
			Next: compile(t, mapping.ContextPage, "response.body.next", mapping.OutputString),
		}
		if _, err := newExecutor(budgettest.Passthrough{}).Run(context.Background(), job, start0(), time.Time{}); err != nil {
			t.Fatal(err)
		}
		srv.Close()
		if diff := cmp.Diff([]string{"", "c1", "c2"}, seen); diff != "" {
			t.Errorf("header %v: cursors (-want +got):\n%s", viaHeader, diff)
		}
	}
}

func TestRunExpressionPagination(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.RawQuery)
		from, _ := strconv.Atoi(r.URL.Query().Get("from"))
		fmt.Fprintf(w, `{"from": %d, "more": %v}`, from, from < 20)
	}))
	defer srv.Close()
	job := newJob(t, srv.URL, nil)
	job.Pagination = runtime.Pagination{
		Type: config.PaginationExpression, MaxPages: 100,
		Next: compile(t, mapping.ContextPage, "response.body.more ? {'query': {'from': response.body.from + 10}} : dyn(null)", mapping.OutputMap),
	}
	if _, err := newExecutor(budgettest.Passthrough{}).Run(context.Background(), job, start0(), time.Time{}); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]string{"", "from=10", "from=20"}, seen); diff != "" {
		t.Errorf("requests (-want +got):\n%s", diff)
	}

	job.Pagination.Next = compile(t, mapping.ContextPage, "{'other': 1}", mapping.OutputMap)
	if _, err := newExecutor(budgettest.Passthrough{}).Run(context.Background(), job, start0(), time.Time{}); err == nil ||
		!strings.Contains(err.Error(), "pagination next must return {query: {...}} or null") {
		t.Errorf("bad shape error = %v", err)
	}
}

func start0() time.Time { return start }

// observer records the events of an executor.
type observer struct {
	mu       sync.Mutex
	events   []string
	inFlight int
}

func (o *observer) Request(provider, account, job, endpoint string, status int, class string, _ time.Duration, size int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.events = append(o.events, fmt.Sprintf("request %s %s %q %s %d %q %d", provider, account, job, endpoint, status, class, size))
}

func (o *observer) Retry(provider, account, job, reason string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.events = append(o.events, fmt.Sprintf("retry %s %s %s %s", provider, account, job, reason))
}

func (o *observer) InFlight(_, _ string, delta int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.inFlight += delta
	if o.inFlight < 0 || o.inFlight > 1 {
		o.events = append(o.events, fmt.Sprintf("in flight %d", o.inFlight))
	}
}

func TestObserver(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		switch {
		case r.URL.Path == "/token":
			w.WriteHeader(http.StatusUnauthorized)
		case n == 1:
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			fmt.Fprint(w, `[1, 2]`)
		}
	}))
	defer srv.Close()
	clk := clock.NewFake(start)
	autoAdvance(t, clk, time.Minute)
	obs := &observer{}
	e := New(Options{
		Budget: &recorder{}, Clock: clk, Observer: obs, Rand: rand.New(rand.NewPCG(1, 2)),
		Retry: config.RetrySettings{MaxAttempts: 3, InitialBackoff: config.Duration(time.Second), Multiplier: 2},
	})
	if _, err := e.Run(context.Background(), newJob(t, srv.URL, nil), start, time.Time{}); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+"/token", http.NoBody)
	resp, err := e.SendAuth(context.Background(), auth.Scope{Provider: "example", Account: "acme"}, "/token", req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	srv.Close()
	req, _ = http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+"/token", http.NoBody)
	if resp, err := e.SendAuth(context.Background(), auth.Scope{Provider: "example", Account: "acme"}, "/token", req); err == nil {
		resp.Body.Close()
		t.Fatal("SendAuth to a closed server succeeded")
	}
	want := []string{
		`request example acme "devices" /devices 503 "transient" 0`,
		`retry example acme devices transient`,
		`request example acme "devices" /devices 200 "" 6`,
		`request example acme "" /token 401 "auth" -1`,
		`request example acme "" /token 0 "transient" 0`,
	}
	obs.mu.Lock()
	defer obs.mu.Unlock()
	if diff := cmp.Diff(want, obs.events); diff != "" {
		t.Errorf("events (-want +got):\n%s", diff)
	}
	if obs.inFlight != 0 {
		t.Errorf("%d requests still in flight", obs.inFlight)
	}
}

func TestStatusClass(t *testing.T) {
	for status, want := range map[int]Class{
		200: "", 302: "", 401: ClassAuth, 403: ClassAuth, 429: ClassRateLimited, 500: ClassTransient, 503: ClassTransient, 400: ClassClient,
	} {
		if got := statusClass(status); got != want {
			t.Errorf("statusClass(%d) = %q, want %q", status, got, want)
		}
	}
}

// TestFailedRunResult: a failed run still tells what it did, so that its
// record and its debug capture are not empty.
func TestFailedRunResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error": "bad"}`))
	}))
	defer srv.Close()
	res, err := newExecutor(&recorder{}).Run(context.Background(), newJob(t, srv.URL, nil), start, time.Time{})
	if err == nil {
		t.Fatal("Run() succeeded")
	}
	if res == nil || res.Requests != 1 || len(res.Pages) != 0 {
		t.Errorf("result of the failed run = %+v, want 1 request and no page", res)
	}
}
