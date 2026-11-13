// Package sim is the provider simulator: an HTTP server driven by a provider
// package, serving its fixtures with the auth, pagination and limits of the
// same Provider definition (see docs/spec/08-simulator.md). Limits are
// enforced by the budget engine in enforcer mode; scenarios overlay quota
// changes, external consumption and errors on a timeline.
package sim

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"text/template"
	"time"

	"github.com/kentrow/tapyr/internal/auth"
	"github.com/kentrow/tapyr/internal/budget"
	"github.com/kentrow/tapyr/internal/clock"
	"github.com/kentrow/tapyr/internal/config"
	"github.com/kentrow/tapyr/internal/mapping"
)

// pageParam is the opaque query parameter of the simulator's Link header
// pagination.
const pageParam = "page"

// CursorHeader carries the next cursor of cursor pagination; job templates
// read it with default(response.headers['x-next-cursor'][0], ”).
const CursorHeader = "X-Next-Cursor"

// Options configures a Server.
type Options struct {
	Logger *slog.Logger
	// Clock drives windows, refills and the scenario timeline.
	Clock clock.Clock
	// Scenario overlays faults; nil for the default behavior.
	Scenario *config.Scenario
	// Seed makes injected errors reproducible.
	Seed uint64
	// Plugins serve the auth of the providers whose auth is a plugin;
	// required for those (cmd/tapyr passes the built-in plugins).
	Plugins auth.Plugins
}

// Server serves a provider package.
type Server struct {
	pkg      *config.Package
	basePath string
	routes   []*route
	accept   func(*http.Request) (string, bool)
	issuer   *issuer
	plugin   auth.PluginSimulator // nil unless the auth is a plugin
	authFail int
	logger   *slog.Logger
	clock    clock.Clock
	engine   *budget.Engine
	scenario *config.Scenario
	started  time.Time
	headers  map[string]string
	requests atomic.Int64

	inv *inventory

	mu        sync.Mutex
	fixtures  map[string]any
	templates map[string]*template.Template
	rand      *rand.Rand
	external  map[string]float64 // limit -> fractional units not consumed yet
	expired   map[int]bool       // tokenExpiry steps already applied
	lastTick  time.Time
	stats     Stats
}

// Stats counts what the simulator served.
type Stats struct {
	Requests int64            `json:"requests"`
	Served   int64            `json:"served"`
	Refused  map[string]int64 `json:"rate_limited"` // by reason: quota_exhausted, rate, concurrency
	Injected int64            `json:"injected_errors"`
	// TokensIssued counts the tokens issued by the token or login endpoint.
	TokensIssued int64 `json:"tokens_issued"`
	// Dropped counts the connections closed by an outage.
	Dropped int64 `json:"dropped"`
	// LoggedOut counts the sessions closed by the logout endpoint.
	LoggedOut int64 `json:"logged_out"`
	// OpenSessions counts the issued tokens that have not expired nor been
	// logged out.
	OpenSessions int64            `json:"open_sessions"`
	Routes       map[string]int64 `json:"routes"` // granted requests per route
}

type route struct {
	method     string
	path       string
	segments   []string
	pagination *config.Pagination
	status     int
	headers    map[string]string
	file       string
	template   bool     // file is a template fixture
	items      []string // itemsField, split
}

// New returns a simulator for a package with a sim/sim.yaml.
func New(pkg *config.Package, opts Options) (*Server, error) {
	name := pkg.Provider.Metadata.Name
	if pkg.Simulation == nil {
		return nil, fmt.Errorf("provider %s has no simulator (%s)", name, config.PackageSimulationFile)
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Clock == nil {
		opts.Clock = clock.Real()
	}
	spec := &pkg.Simulation.Spec
	engine, err := budget.NewEngine(budget.Config{Providers: []*config.Provider{pkg.Provider}}, opts.Clock, opts.Logger)
	if err != nil {
		return nil, fmt.Errorf("provider %s simulator: %w", name, err)
	}
	s := &Server{
		pkg: pkg, basePath: strings.TrimSuffix(spec.BasePath, "/"), logger: opts.Logger, fixtures: map[string]any{},
		templates: map[string]*template.Template{},
		clock:     opts.Clock, engine: engine, scenario: opts.Scenario, started: opts.Clock.Now(), headers: spec.Headers,
		rand:     rand.New(rand.NewPCG(opts.Seed, opts.Seed^0x9e3779b97f4a7c15)), //nolint:gosec // G404: reproducible simulated faults
		external: map[string]float64{}, expired: map[int]bool{},
		stats: Stats{Refused: map[string]int64{}, Routes: map[string]int64{}},
	}
	s.lastTick = s.started
	if s.inv, err = newInventory(spec.Inventory); err != nil {
		return nil, fmt.Errorf("provider %s simulator: %w", name, err)
	}
	if err := s.checkHeaders(); err != nil {
		return nil, fmt.Errorf("provider %s simulator: %w", name, err)
	}
	if s.issuer, err = s.newIssuer(pkg.Provider.Spec.Auth, spec.Auth); err != nil {
		return nil, fmt.Errorf("provider %s simulator: %w", name, err)
	}
	if a := pkg.Provider.Spec.Auth; a != nil && a.Plugin != nil {
		pa := a.Plugin
		if opts.Plugins == nil {
			return nil, fmt.Errorf("provider %s simulator: Options.Plugins is required for the auth plugin %s", name, pa.Name)
		}
		if s.plugin, err = newPluginSimulator(pa, spec, opts); err != nil {
			return nil, fmt.Errorf("provider %s simulator: %w", name, err)
		}
	}
	accept, err := acceptor(pkg.Provider.Spec.Auth, spec.Auth, s.issuer, s.plugin)
	if err != nil {
		return nil, fmt.Errorf("provider %s simulator: %w", name, err)
	}
	s.accept = accept
	s.authFail = http.StatusUnauthorized
	if e := pkg.Provider.Spec.Errors; e != nil && e.AuthFailed != nil && len(e.AuthFailed.Statuses) > 0 {
		s.authFail = e.AuthFailed.Statuses[0]
	}
	var errs []error
	for i := range spec.Routes {
		r, err := s.newRoute(&spec.Routes[i])
		if err != nil {
			errs = append(errs, fmt.Errorf("route %s %s: %w", spec.Routes[i].Method, spec.Routes[i].Path, err))
			continue
		}
		s.routes = append(s.routes, r)
	}
	errs = append(errs, s.checkScenarioFixtures()...)
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("provider %s simulator: %w", name, err)
	}
	return s, nil
}

// checkScenarioFixtures checks that the fixtures replaced by the scenario
// belong to routes and, when static, load like the route fixtures.
func (s *Server) checkScenarioFixtures() []error {
	if s.scenario == nil {
		return nil
	}
	var errs []error
	for i, st := range s.scenario.Spec.Steps {
		if l := st.Latency; l != nil && l.P99 != 0 && l.P99 < l.P50 {
			errs = append(errs, fmt.Errorf("scenario %s step %d: latency: p99 is below p50", s.scenario.Metadata.Name, i))
		}
		paths := make([]string, 0, len(st.Fixtures))
		for p := range st.Fixtures {
			paths = append(paths, p)
		}
		slices.Sort(paths)
		for _, routePath := range paths {
			file := st.Fixtures[routePath]
			idx := slices.IndexFunc(s.routes, func(r *route) bool { return r.path == routePath })
			if idx < 0 {
				errs = append(errs, fmt.Errorf("scenario %s step %d: fixtures: no route %s", s.scenario.Metadata.Name, i, routePath))
				continue
			}
			if strings.Contains(file, "{") {
				continue
			}
			var err error
			if strings.HasSuffix(file, ".tmpl") {
				_, err = s.parsed(file)
			} else {
				var v any
				v, err = s.fixture(file)
				if _, ok := listAt(v, s.routes[idx].items); err == nil && s.routes[idx].pagination != nil && !ok {
					err = fmt.Errorf("fixture %s has no list to paginate", file)
				}
			}
			if err != nil {
				errs = append(errs, fmt.Errorf("scenario %s step %d: %w", s.scenario.Metadata.Name, i, err))
			}
		}
	}
	return errs
}

// fixtureOf returns the fixture file of a route now: the route file, or
// the one of the last active scenario step replacing it (a template when
// its name ends in .tmpl).
func (s *Server) fixtureOf(r *route, now time.Time) (string, bool) {
	file, templated := r.file, r.template
	for _, st := range s.activeSteps(now) {
		if f, ok := st.Fixtures[r.path]; ok {
			file, templated = f, strings.HasSuffix(f, ".tmpl")
		}
	}
	return file, templated
}

func (s *Server) newRoute(r *config.SimulationRoute) (*route, error) {
	out := &route{
		method: r.Method, path: r.Path, segments: strings.Split(strings.Trim(r.Path, "/"), "/"),
		status: r.Response.Status, headers: r.Response.Headers, file: r.Response.File, items: splitField(r.Response.ItemsField),
	}
	if r.Response.Template != "" {
		out.file, out.template = r.Response.Template, true
	}
	if out.method == "" {
		out.method = http.MethodGet
	}
	if out.status == 0 {
		out.status = http.StatusOK
	}
	switch r.Pagination {
	case "", config.PaginationNone:
	case config.PaginationOffset, config.PaginationPage, config.PaginationLinkHeader, config.PaginationCursor:
		p, err := s.paginationFor(out.method, r.Path, r.Pagination)
		if err != nil {
			return nil, err
		}
		out.pagination = p
	default:
		return nil, fmt.Errorf("pagination %s is not served by the simulator (it is computed by the client)", r.Pagination)
	}
	if out.items != nil && out.pagination == nil {
		return nil, errors.New("itemsField needs a pagination")
	}
	// Static fixtures are checked at startup, templates are parsed;
	// fixtures with placeholders are checked when they are requested.
	if strings.Contains(out.file, "{") {
		return out, nil
	}
	if out.template {
		if _, err := s.parsed(out.file); err != nil {
			return nil, err
		}
		return out, nil
	}
	v, err := s.fixture(out.file)
	if err != nil {
		return nil, err
	}
	if _, ok := listAt(v, out.items); out.pagination != nil && !ok {
		if out.items != nil {
			return nil, fmt.Errorf("fixture %s has no list at %s", out.file, r.Response.ItemsField)
		}
		return nil, fmt.Errorf("fixture %s must be a JSON array for a paginated route", out.file)
	}
	return out, nil
}

// paginationFor finds the pagination parameters of a route: from the job
// templates requesting that path, else from the provider default.
func (s *Server) paginationFor(method, routePath string, typ config.PaginationType) (*config.Pagination, error) {
	for _, t := range s.pkg.JobTemplates {
		r := t.Spec.Request
		if r == nil || r.Path != routePath || (r.Method != "" && r.Method != method) {
			continue
		}
		if p := t.Spec.Pagination; p != nil && p.Type == typ {
			return p, nil
		}
	}
	if p := s.pkg.Provider.Spec.Pagination; p != nil && p.Type == typ {
		return p, nil
	}
	return nil, fmt.Errorf("no %s pagination declared for this path by the job templates or the provider", typ)
}

func (s *Server) fixture(file string) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.fixtures[file]; ok {
		return v, nil
	}
	data, err := fs.ReadFile(s.pkg.FS, path.Join("sim", file))
	if err != nil {
		return nil, fmt.Errorf("fixture %s: %w", file, unwrapPathError(err))
	}
	v, err := mapping.DecodeJSON(data)
	if err != nil {
		return nil, fmt.Errorf("fixture %s: %w", file, err)
	}
	s.fixtures[file] = v
	return v, nil
}

// Handler serves the simulated API and the /_sim/ endpoints.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /_sim/status", s.status)
	mux.HandleFunc("/", s.serve)
	return mux
}

func (s *Server) status(w http.ResponseWriter, _ *http.Request) {
	routes := make([]string, len(s.routes))
	for i, r := range s.routes {
		routes[i] = r.method + " " + s.basePath + r.path
	}
	scenario := ""
	if s.scenario != nil {
		scenario = string(s.scenario.Metadata.Name)
	}
	s.applyScenario()
	writeJSON(w, http.StatusOK, nil, map[string]any{
		"provider": s.pkg.Provider.Metadata.Name,
		"version":  s.pkg.Provider.Metadata.Version,
		"routes":   routes,
		"requests": s.requests.Load(),
		"time":     s.clock.Now().UTC().Format(time.RFC3339),
		"scenario": scenario,
		"stats":    s.Stats(),
		"quotas":   s.engine.Quotas(),
	})
}

// Stats returns a copy of the counters.
func (s *Server) Stats() Stats {
	open := s.issuer.open()
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.stats
	out.OpenSessions = open
	if s.plugin != nil {
		out.TokensIssued += s.plugin.Issued()
	}
	out.Refused = map[string]int64{}
	out.Routes = map[string]int64{}
	for k, v := range s.stats.Refused {
		out.Refused[k] = v
	}
	for k, v := range s.stats.Routes {
		out.Routes[k] = v
	}
	return out
}

func (s *Server) count(f func(*Stats)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(&s.stats)
}

// activeSteps returns the scenario steps in effect now.
func (s *Server) activeSteps(now time.Time) []*config.ScenarioStep {
	if s.scenario == nil {
		return nil
	}
	elapsed := now.Sub(s.started)
	var out []*config.ScenarioStep
	for i := range s.scenario.Spec.Steps {
		st := &s.scenario.Spec.Steps[i]
		if elapsed >= st.At.Std() && (st.For == 0 || elapsed < st.At.Std()+st.For.Std()) {
			out = append(out, st)
		}
	}
	return out
}

// applyScenario sets the quota overrides of the active steps and consumes
// the external consumption accumulated since the last call.
func (s *Server) applyScenario() {
	if s.scenario == nil {
		return
	}
	provider := string(s.pkg.Provider.Metadata.Name)
	now := s.clock.Now()
	overrides := map[string]int64{}
	for i := range s.scenario.Spec.Steps {
		for name := range s.scenario.Spec.Steps[i].Limits {
			overrides[name] = -1
		}
	}
	s.mu.Lock()
	last := s.lastTick
	s.lastTick = now
	s.mu.Unlock()
	for _, st := range s.activeSteps(now) {
		for name, l := range st.Limits {
			overrides[name] = l.Max
		}
	}
	// External consumption since the last call, also for the steps that
	// ended since: a step ending between two calls still consumed until its
	// end.
	for i := range s.scenario.Spec.Steps {
		st := &s.scenario.Spec.Steps[i]
		ec := st.ExternalConsumption
		if ec == nil || !now.After(last) {
			continue
		}
		start := s.started.Add(st.At.Std())
		from, to := last, now
		if start.After(from) {
			from = start
		}
		if end := start.Add(st.For.Std()); st.For > 0 && end.Before(to) {
			to = end
		}
		// Only the consumption of the current window counts: the rest went
		// into windows that have reset since, and charging it now would
		// exhaust a short window after a quiet spell.
		window := s.engine.QuotaWindowStart(provider, string(ec.Limit))
		s.mu.Lock()
		if from.Before(window) {
			from = window
			s.external[string(ec.Limit)] = 0
		}
		if !to.After(from) {
			s.mu.Unlock()
			continue
		}
		s.external[string(ec.Limit)] += ec.RatePerHour * to.Sub(from).Hours()
		units := int64(s.external[string(ec.Limit)])
		s.external[string(ec.Limit)] -= float64(units)
		s.mu.Unlock()
		if units > 0 {
			s.engine.Consume(provider, string(ec.Limit), units)
		}
	}
	for name, m := range overrides {
		s.engine.SetQuotaMax(provider, name, m)
	}
	for i := range s.scenario.Spec.Steps {
		st := &s.scenario.Spec.Steps[i]
		start := s.started.Add(st.At.Std())
		s.mu.Lock()
		due := st.TokenExpiry && !s.expired[i] && !now.Before(start)
		if due {
			s.expired[i] = true
		}
		s.mu.Unlock()
		if due {
			s.issuer.expireBefore(start)
			if s.plugin != nil {
				s.plugin.ExpireBefore(start)
			}
		}
	}
}

// fault returns the latency of the active steps (summed draws), and
// whether an outage is in effect.
func (s *Server) fault(now time.Time) (time.Duration, bool) {
	var delay time.Duration
	for _, st := range s.activeSteps(now) {
		if st.Outage {
			return 0, true
		}
		if l := st.Latency; l != nil {
			delay += s.latency(l.P50.Std(), l.P99.Std())
		}
	}
	return delay, false
}

// latency draws a log-normal delay with median p50 and 99th percentile
// p99.
func (s *Server) latency(p50, p99 time.Duration) time.Duration {
	if p50 <= 0 {
		return 0
	}
	sigma := 0.0
	if p99 > p50 {
		sigma = math.Log(float64(p99)/float64(p50)) / 2.326348 // z of the 99th percentile
	}
	s.mu.Lock()
	z := s.rand.NormFloat64()
	s.mu.Unlock()
	return time.Duration(float64(p50) * math.Exp(sigma*z))
}

// drop closes the connection of a request without a response.
func drop(w http.ResponseWriter) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		panic(http.ErrAbortHandler)
	}
	conn, _, err := hj.Hijack()
	if err != nil {
		panic(http.ErrAbortHandler)
	}
	_ = conn.Close()
}

// injectedError returns the status of an injected error for the route, or 0.
func (s *Server) injectedError(r *route, now time.Time) int {
	for _, st := range s.activeSteps(now) {
		e := st.Errors
		if e == nil {
			continue
		}
		if len(e.Routes) > 0 && !slices.ContainsFunc(e.Routes, func(p string) bool { return budget.GlobMatch(p, r.path) }) {
			continue
		}
		s.mu.Lock()
		hit := s.rand.Float64() < e.Ratio
		s.mu.Unlock()
		if hit {
			return e.Status
		}
	}
	return 0
}

// checkHeaders validates the dynamic header definitions.
func (s *Server) checkHeaders() error {
	for name, def := range s.headers {
		limitName, field, err := parseHeaderDef(def)
		if err != nil {
			return fmt.Errorf("header %s: %w", name, err)
		}
		found := slices.ContainsFunc(s.pkg.Provider.Spec.Limits, func(l config.Limit) bool {
			return string(l.Name) == limitName && l.Kind == config.LimitQuota
		})
		if !found {
			return fmt.Errorf("header %s: no quota limit %q (%s)", name, limitName, field)
		}
	}
	return nil
}

func parseHeaderDef(def string) (limitName, field string, err error) {
	rest, ok := strings.CutPrefix(def, "limit:")
	limitName, field, dot := strings.Cut(rest, ".")
	if !ok || !dot || (field != "max" && field != "remaining" && field != "reset") {
		return "", "", fmt.Errorf("invalid definition %q (want limit:<name>.max, .remaining or .reset)", def)
	}
	return limitName, field, nil
}

// quotaHeaders computes the dynamic headers for a request.
func (s *Server) quotaHeaders(d budget.Descriptor, headers map[string]string) {
	now := s.clock.Now()
	for name, def := range s.headers {
		limitName, field, _ := parseHeaderDef(def)
		q, ok := s.engine.QuotaOf(d, limitName)
		if !ok {
			continue
		}
		switch field {
		case "max":
			headers[name] = strconv.FormatInt(q.Max, 10)
		case "remaining":
			headers[name] = strconv.FormatInt(q.Remaining, 10)
		case "reset":
			headers[name] = strconv.FormatInt(int64(math.Ceil(q.WindowEnd.Sub(now).Seconds())), 10)
		}
	}
}

func (s *Server) serve(w http.ResponseWriter, req *http.Request) {
	s.requests.Add(1)
	s.count(func(st *Stats) { st.Requests++ })
	delay, outage := s.fault(s.clock.Now())
	if outage {
		s.count(func(st *Stats) { st.Dropped++ })
		drop(w)
		return
	}
	// The latency of the scenario is the processing time of the server: a
	// request is counted by the limits when it arrives, then answered after
	// the delay, holding its concurrency slot. Counted after the delay,
	// requests sent at the pace of the limit would reach the limits bunched
	// together and get 429 the client could not foresee. A refusal answers
	// at once, as a rate limiter in front of the server does.
	wait := func() bool {
		return delay <= 0 || s.clock.Sleep(req.Context(), delay) == nil
	}
	if s.issuer.handles(req) {
		s.issuer.serve(w, req, wait)
		return
	}
	if s.plugin != nil {
		if endpoint, identity, handled, ok := s.plugin.TokenRequest(req); handled {
			s.servePluginToken(w, req, endpoint, identity, ok, wait)
			return
		}
	}
	rel, ok := strings.CutPrefix(req.URL.Path, s.basePath)
	if !ok || (rel != "" && !strings.HasPrefix(rel, "/")) {
		if wait() {
			writeError(w, http.StatusNotFound, "not found")
		}
		return
	}
	r, vars := s.match(req.Method, rel)
	if r == nil {
		if wait() {
			writeError(w, http.StatusNotFound, "not found")
		}
		return
	}
	s.applyScenario() // before the credentials: tokenExpiry
	identity, ok := s.accept(req)
	if !ok {
		if wait() {
			writeError(w, s.authFail, "invalid credentials")
		}
		return
	}
	d := budget.Descriptor{
		Provider: string(s.pkg.Provider.Metadata.Name), Account: identity, Target: identity, Credential: identity,
		Endpoint: r.path, Method: req.Method, Vars: vars,
	}
	release, refused := s.engine.Check(d)
	if refused != nil {
		retryAfter := max(1, int64(math.Ceil(refused.Until.Sub(s.clock.Now()).Seconds())))
		s.count(func(st *Stats) { st.Refused[refused.Reason]++ })
		headers := map[string]string{"Retry-After": strconv.FormatInt(retryAfter, 10)}
		s.quotaHeaders(d, headers)
		writeJSON(w, http.StatusTooManyRequests, headers, map[string]string{"error": "rate limited", "reason": refused.Reason})
		return
	}
	defer release()
	if !wait() {
		return
	}
	s.count(func(st *Stats) { st.Routes[r.method+" "+r.path]++ })
	if status := s.injectedError(r, s.clock.Now()); status != 0 {
		s.count(func(st *Stats) { st.Injected++ })
		headers := map[string]string{}
		s.quotaHeaders(d, headers)
		writeJSON(w, status, headers, map[string]string{"error": "injected by the scenario"})
		return
	}
	file, templated := s.fixtureOf(r, s.clock.Now())
	for k, v := range vars {
		file = strings.ReplaceAll(file, "{"+k+"}", v)
	}
	var body any
	var err error
	if templated {
		body, err = s.render(file, vars, req.URL.Query())
	} else {
		body, err = s.fixture(file)
	}
	if err != nil {
		s.logger.Warn("fixture", "component", "sim", "error", err)
		// A missing fixture is an unknown object (404). A fixture that does
		// not render or parse is a fault of the package: the client, a
		// contract test or its author, sees the cause in the response.
		if errors.Is(err, fs.ErrNotExist) {
			writeError(w, http.StatusNotFound, "not found")
		} else {
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	headers := maps(r.headers)
	s.quotaHeaders(d, headers)
	if r.pagination != nil {
		items, ok := listAt(body, r.items)
		if !ok {
			writeError(w, http.StatusInternalServerError, "paginated fixture has no list")
			return
		}
		page, err := paginate(req, r.pagination, items, headers)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		body = withList(body, r.items, page.([]any))
	}
	s.logger.Debug("request", "component", "sim", "method", req.Method, "path", req.URL.Path, "status", r.status)
	s.count(func(st *Stats) { st.Served++ })
	writeJSON(w, r.status, headers, body)
}

// newPluginSimulator returns the simulator side of the auth plugin of the
// provider.
func newPluginSimulator(pa *config.PluginAuth, spec *config.SimulationSpec, opts Options) (auth.PluginSimulator, error) {
	p, err := opts.Plugins.Get(pa.Name)
	if err != nil {
		return nil, err
	}
	env := auth.SimulatorEnv{Clock: opts.Clock, BasePath: strings.TrimSuffix(spec.BasePath, "/")}
	var accept map[string]any
	if sa := spec.Auth; sa != nil {
		env.TokenLifetime, accept = sa.TokenLifetime.Std(), sa.Accept.Plugin
	}
	return p.Simulator(env, accept)
}

// servePluginToken answers a token request of an auth plugin, as an auth
// request of the provider limits.
func (s *Server) servePluginToken(w http.ResponseWriter, req *http.Request, endpoint, identity string, ok bool, wait func() bool) {
	if !ok {
		if wait() {
			writeError(w, s.authFail, "invalid client credentials")
		}
		return
	}
	s.applyScenario()
	d := budget.Descriptor{
		Provider: string(s.pkg.Provider.Metadata.Name), Account: identity, Target: identity, Credential: identity,
		Endpoint: endpoint, Method: req.Method, AuthRequest: true,
	}
	release, refused := s.engine.Check(d)
	if refused != nil {
		retryAfter := max(1, int64(math.Ceil(refused.Until.Sub(s.clock.Now()).Seconds())))
		s.count(func(st *Stats) { st.Refused[refused.Reason]++ })
		headers := map[string]string{"Retry-After": strconv.FormatInt(retryAfter, 10)}
		s.quotaHeaders(d, headers)
		writeJSON(w, http.StatusTooManyRequests, headers, map[string]string{"error": "rate limited", "reason": refused.Reason})
		return
	}
	defer release()
	if !wait() {
		return
	}
	s.count(func(st *Stats) { st.Routes[req.Method+" "+endpoint]++; st.Served++ })
	headers := map[string]string{}
	s.quotaHeaders(d, headers)
	for k, v := range headers {
		w.Header().Set(k, v)
	}
	s.plugin.ServeToken(w, req, identity)
}

// match returns the route of a path relative to the base path, and the
// values of its {var} segments.
func (s *Server) match(method, rel string) (*route, map[string]string) {
	segs := strings.Split(strings.Trim(rel, "/"), "/")
	for _, r := range s.routes {
		if r.method != method || len(r.segments) != len(segs) {
			continue
		}
		vars := map[string]string{}
		ok := true
		for i, rs := range r.segments {
			switch {
			case strings.HasPrefix(rs, "{") && strings.HasSuffix(rs, "}"):
				v, err := url.PathUnescape(segs[i])
				if err != nil {
					ok = false
				}
				vars[rs[1:len(rs)-1]] = v
			case rs != segs[i]:
				ok = false
			}
		}
		if ok {
			return r, vars
		}
	}
	return nil, nil
}

func paginate(req *http.Request, p *config.Pagination, items []any, headers map[string]string) (any, error) {
	q := req.URL.Query()
	intParam := func(name string, def int) (int, error) {
		v := q.Get(name)
		if v == "" || name == "" {
			return def, nil
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("invalid %s parameter %q", name, v)
		}
		return n, nil
	}
	switch p.Type {
	case config.PaginationOffset:
		o := p.Offset
		offset, err := intParam(o.OffsetParam, o.Start)
		if err != nil {
			return nil, err
		}
		limit, err := intParam(o.LimitParam, o.PageSize)
		if err != nil {
			return nil, err
		}
		return window(items, offset, limit), nil
	case config.PaginationPage:
		pp := p.Page
		first := 1
		if pp.Start != nil {
			first = *pp.Start
		}
		size := pp.PageSize
		if size <= 0 {
			size = 100
		}
		size, err := intParam(pp.SizeParam, size)
		if err != nil || size == 0 {
			return nil, fmt.Errorf("invalid page size")
		}
		number, err := intParam(pp.PageParam, first)
		if err != nil || number < first {
			return nil, fmt.Errorf("invalid page number")
		}
		return window(items, (number-first)*size, size), nil
	case config.PaginationCursor:
		c := p.Cursor
		size := c.PageSize
		if size <= 0 {
			size = 100
		}
		size, err := intParam(c.PageSizeParam, size)
		if err != nil || size == 0 {
			return nil, fmt.Errorf("invalid page size")
		}
		cursor := q.Get(c.Param)
		if c.Header != "" {
			cursor = req.Header.Get(c.Header)
		}
		offset := 0
		if cursor != "" {
			n, err := strconv.Atoi(strings.TrimPrefix(cursor, "o"))
			if err != nil || !strings.HasPrefix(cursor, "o") || n < 0 {
				return nil, fmt.Errorf("invalid cursor %q", cursor)
			}
			offset = n
		}
		if offset+size < len(items) {
			headers[CursorHeader] = "o" + strconv.Itoa(offset+size)
		}
		return window(items, offset, size), nil
	case config.PaginationLinkHeader:
		size := 100
		sizeParam := ""
		if lh := p.LinkHeader; lh != nil {
			sizeParam = lh.PageSizeParam
			if lh.PageSize > 0 {
				size = lh.PageSize
			}
		}
		size, err := intParam(sizeParam, size)
		if err != nil || size == 0 {
			return nil, fmt.Errorf("invalid page size")
		}
		page, err := intParam(pageParam, 1)
		if err != nil || page == 0 {
			return nil, fmt.Errorf("invalid page")
		}
		if page*size < len(items) {
			next := *req.URL
			nq := next.Query()
			nq.Set(pageParam, strconv.Itoa(page+1))
			next.RawQuery = nq.Encode()
			next.Scheme, next.Host = "http", req.Host
			if req.TLS != nil {
				next.Scheme = "https"
			}
			headers["Link"] = fmt.Sprintf(`<%s>; rel="next"`, next.String())
		}
		return window(items, (page-1)*size, size), nil
	}
	return items, nil
}

func window(items []any, offset, limit int) []any {
	if offset >= len(items) {
		return []any{}
	}
	return items[offset:min(offset+limit, len(items))]
}

// acceptor returns the credential check of the provider auth type. It
// returns the identity of the client (its credential) for scope keys.
func acceptor(a *config.Auth, sa *config.SimulationAuth, is *issuer, plugin auth.PluginSimulator) (func(*http.Request) (string, bool), error) {
	var accept config.SimulationCredentials
	if sa != nil {
		accept = sa.Accept
	}
	typ := config.AuthNone
	if a != nil {
		typ = a.Type
	}
	switch typ {
	case config.AuthNone:
		return func(*http.Request) (string, bool) { return "anonymous", true }, nil
	case config.AuthBearer:
		return func(r *http.Request) (string, bool) {
			token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			return token, ok && oneOf(token, accept.Bearer)
		}, nil
	case config.AuthHeader:
		name := a.Header.Name
		accepted := make([]string, len(accept.Header))
		for i, v := range accept.Header {
			accepted[i] = a.Header.Formatted(v)
		}
		return func(r *http.Request) (string, bool) {
			v := r.Header.Get(name)
			return v, oneOf(v, accepted)
		}, nil
	case config.AuthBasic:
		return func(r *http.Request) (string, bool) {
			user, pass, ok := r.BasicAuth()
			return user, ok && slices.ContainsFunc(accept.Basic, func(c config.SimulationBasicCredential) bool {
				return equal(user, c.Username) && equal(pass, c.Password)
			})
		}, nil
	case config.AuthOAuth2ClientCredentials, config.AuthSessionLogin:
		return func(r *http.Request) (string, bool) { return is.valid(is.tokenOf(r)) }, nil
	case config.AuthPlugin:
		return plugin.Identity, nil
	}
	return nil, fmt.Errorf("auth type %s is not supported by the simulator", typ)
}

func oneOf(v string, accepted []string) bool {
	return v != "" && slices.ContainsFunc(accepted, func(a string) bool { return equal(v, a) })
}

func equal(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

func maps(m map[string]string) map[string]string {
	out := make(map[string]string, len(m)+1)
	for k, v := range m {
		out[k] = v
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, headers map[string]string, v any) {
	for k, val := range headers {
		w.Header().Set(k, val)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, nil, map[string]string{"error": msg})
}

func unwrapPathError(err error) error {
	if pe, ok := errors.AsType[*fs.PathError](err); ok {
		return pe.Err
	}
	return err
}
