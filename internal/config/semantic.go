package config

import (
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// maxDiscoveryDepth is the maximum number of jobs in a discovery chain, from
// the root inventory job to the deepest child.
const maxDiscoveryDepth = 4

// checker accumulates semantic diagnostics.
type checker struct {
	cfg       *Config
	diags     Diagnostics
	warnings  Diagnostics
	providers map[string]*Provider
	templates map[string]*JobTemplate
	accounts  map[string]*Account
	jobs      map[string]*Job
	outputs   map[string]*Output
	// metricTypes records the first declaration of every metric name.
	metricTypes map[string]metricDecl
}

type metricDecl struct {
	typ MetricType
	src *Source
	at  []string
}

// at locates a value of a document.
type at struct {
	src    *Source
	tokens []string
}

func loc(src *Source, tokens ...string) at { return at{src: src, tokens: tokens} }

func (a at) child(tokens ...string) at {
	return at{src: a.src, tokens: append(slices.Clip(a.tokens), tokens...)}
}

func (c *checker) errorf(a at, format string, args ...any) {
	c.diags = append(c.diags, Diagnostic{
		Pos:     a.src.positions.position(a.tokens, false),
		Path:    a.src.positions.path(a.tokens),
		Message: fmt.Sprintf(format, args...),
	})
}

func (c *checker) warnf(a at, format string, args ...any) {
	c.warnings = append(c.warnings, Diagnostic{
		Pos:     a.src.positions.position(a.tokens, false),
		Path:    a.src.positions.path(a.tokens),
		Message: fmt.Sprintf(format, args...),
	})
}

func (c *checker) posOf(a at) Position { return a.src.positions.position(a.tokens, false) }

// validateSemantics checks the rules that span documents or that JSON
// Schema cannot express, and returns the errors and the warnings.
func validateSemantics(cfg *Config) (errs, warnings Diagnostics) {
	c := &checker{cfg: cfg, metricTypes: map[string]metricDecl{}}
	c.providers = index(c, cfg.Providers)
	c.templates = index(c, cfg.JobTemplates)
	c.accounts = index(c, cfg.Accounts)
	c.jobs = index(c, cfg.Jobs)
	c.outputs = index(c, cfg.Outputs)

	for _, p := range cfg.Providers {
		c.checkProvider(p)
	}
	for _, t := range cfg.JobTemplates {
		c.checkTemplate(t)
	}
	for _, a := range cfg.Accounts {
		c.checkAccount(a)
	}
	for _, j := range cfg.Jobs {
		c.checkJob(j)
	}
	c.checkOutputs()
	if cfg.Settings != nil {
		c.checkSettings(cfg.Settings)
	}
	c.checkDiscovery()
	return c.diags, c.warnings
}

// index maps documents by name and reports duplicates.
func index[T envelope](c *checker, docs []T) map[string]T {
	m := make(map[string]T, len(docs))
	for _, d := range docs {
		if first, dup := m[d.name()]; dup {
			c.errorf(loc(d.source(), "metadata", "name"), "duplicate %s name %q (first defined at %s)",
				d.kind(), d.name(), c.posOf(loc(first.source(), "metadata", "name")))
			continue
		}
		m[d.name()] = d
	}
	return m
}

func (c *checker) checkProvider(p *Provider) {
	spec := &p.Spec
	base := loc(p.Source, "spec")
	c.checkTemplateField(base.child("baseURL"), string(spec.BaseURL))
	c.checkUniqueParams(base.child("params"), spec.Params)
	c.checkUniqueParams(base.child("secrets"), spec.Secrets)
	if spec.Defaults != nil {
		for _, h := range sortedKeys(spec.Defaults.Headers) {
			c.checkTemplateField(base.child("defaults", "headers", h), string(spec.Defaults.Headers[h]))
		}
	}
	if spec.Auth != nil {
		c.checkAuth(base.child("auth"), spec.Auth, p)
	}
	if spec.Pagination != nil {
		c.checkPagination(base.child("pagination"), spec.Pagination)
	}
	names := map[Name]int{}
	for i := range spec.Limits {
		l := &spec.Limits[i]
		a := base.child("limits", strconv.Itoa(i))
		if first, dup := names[l.Name]; dup {
			c.errorf(a.child("name"), "duplicate limit name %q (first defined at %s)", l.Name,
				c.posOf(base.child("limits", strconv.Itoa(first), "name")))
		} else {
			names[l.Name] = i
		}
		c.checkLimit(a, l)
	}
	if spec.Redirects != nil {
		for i, h := range spec.Redirects.Hosts {
			if !redirectHostPattern.MatchString(h) {
				c.errorf(base.child("redirects", "hosts", strconv.Itoa(i)),
					"invalid host %q (want a lower-case host name, or \"*.\" and a domain)", h)
			}
		}
	}
}

// redirectHostPattern matches a host name, or "*." and a domain.
var redirectHostPattern = regexp.MustCompile(`^(\*\.)?[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)

func (c *checker) checkUniqueParams(a at, params []ParamSpec) {
	seen := map[Identifier]bool{}
	for i, p := range params {
		if seen[p.Name] {
			c.errorf(a.child(strconv.Itoa(i), "name"), "duplicate name %q", p.Name)
		}
		seen[p.Name] = true
	}
}

func (c *checker) checkTemplateField(a at, text string) {
	if _, err := parseTemplate(a.src.positions.path(a.tokens), text); err != nil {
		c.errorf(a, "invalid template: %v", err)
	}
}

func (c *checker) checkLimit(a at, l *Limit) {
	forbid := func(set bool, field string) {
		if set {
			c.errorf(a.child(field), "%s is not allowed for kind %s", field, l.Kind)
		}
	}
	switch l.Kind {
	case LimitRate:
		if l.Rate != nil && l.Rate.Count <= 0 {
			c.errorf(a.child("rate"), "rate must be greater than zero")
		}
		forbid(l.Max != nil, "max")
		forbid(l.InitialMax != 0, "initialMax")
		forbid(l.Window != nil, "window")
		forbid(l.Sync != nil, "sync")
	case LimitConcurrency:
		if l.Max != nil && l.Max.Auto {
			c.errorf(a.child("max"), `max: auto is only allowed for quota limits`)
		}
		forbid(l.Rate != nil, "rate")
		forbid(l.Burst != 0, "burst")
		forbid(l.InitialMax != 0, "initialMax")
		forbid(l.Window != nil, "window")
		forbid(l.Sync != nil, "sync")
	case LimitQuota:
		forbid(l.Rate != nil, "rate")
		forbid(l.Burst != 0, "burst")
		auto := l.Max != nil && l.Max.Auto
		if auto {
			if l.InitialMax == 0 {
				c.errorf(a.child("max"), "max: auto requires initialMax (used until the limit header is observed)")
			}
			if l.Sync == nil || l.Sync.LimitHeader == "" {
				c.errorf(a.child("max"), "max: auto requires sync.limitHeader")
			}
		} else if l.InitialMax != 0 {
			c.errorf(a.child("initialMax"), "initialMax is only allowed with max: auto")
		}
		if l.Window != nil {
			c.checkWindow(a.child("window"), l.Window)
		}
	}
}

func (c *checker) checkWindow(a at, w *Window) {
	if (w.Period == "") == (w.Rolling == 0) {
		c.errorf(a, "window must set exactly one of period or rolling")
		return
	}
	if w.Rolling != 0 {
		if w.Rolling.Std() < time.Minute {
			c.errorf(a.child("rolling"), "rolling window must be at least 1m (1-minute precision)")
		}
		for field, set := range map[string]bool{"resetAt": w.ResetAt != "", "timezone": w.Timezone != "", "weekStart": w.WeekStart != "", "monthDay": w.MonthDay != 0} {
			if set {
				c.errorf(a.child(field), "%s is only allowed with period", field)
			}
		}
		return
	}
	if w.Timezone != "" {
		if _, err := time.LoadLocation(w.Timezone); err != nil {
			c.errorf(a.child("timezone"), "unknown timezone %q", w.Timezone)
		}
	}
	if w.ResetAt != "" && w.Period != "day" && w.Period != "week" && w.Period != "month" {
		c.errorf(a.child("resetAt"), "resetAt is only allowed for day, week and month periods")
	}
	if w.WeekStart != "" && w.Period != "week" {
		c.errorf(a.child("weekStart"), "weekStart is only allowed for the week period")
	}
	if w.MonthDay != 0 && w.Period != "month" {
		c.errorf(a.child("monthDay"), "monthDay is only allowed for the month period")
	}
}

func (c *checker) checkAuth(a at, auth *Auth, p *Provider) {
	set := map[string]bool{
		"header":                  auth.Header != nil,
		"bearer":                  auth.Bearer != nil,
		"basic":                   auth.Basic != nil,
		"oauth2ClientCredentials": auth.OAuth2ClientCredentials != nil,
		"sessionLogin":            auth.SessionLogin != nil,
		"plugin":                  auth.Plugin != nil,
	}
	for _, block := range sortedKeys(set) {
		if set[block] && authBlocks[auth.Type] != block {
			c.errorf(a.child(block), "%s is not allowed with auth type %s", block, auth.Type)
		}
	}
	secret := func(at at, v Value) {
		if v.Expr != "" {
			// Credentials are resolved once per account, before any
			// request: there is nothing for an expression to read.
			c.errorf(at, "auth values cannot be expressions: use a literal or a {secret: name} reference")
			return
		}
		c.checkSecretRef(at, v, p)
	}
	// The schema guarantees the block of the declared type is present.
	switch {
	case auth.Type == AuthHeader && auth.Header != nil:
		secret(a.child("header", "value"), auth.Header.Value)
		if f := auth.Header.Format; f != "" && !strings.Contains(f, "{value}") {
			c.errorf(a.child("header", "format"), "format must contain {value}")
		}
	case auth.Type == AuthBearer && auth.Bearer != nil:
		secret(a.child("bearer", "token"), auth.Bearer.Token)
	case auth.Type == AuthBasic && auth.Basic != nil:
		secret(a.child("basic", "username"), auth.Basic.Username)
		secret(a.child("basic", "password"), auth.Basic.Password)
	case auth.Type == AuthOAuth2ClientCredentials && auth.OAuth2ClientCredentials != nil:
		o := auth.OAuth2ClientCredentials
		c.checkTemplateField(a.child("oauth2ClientCredentials", "tokenURL"), string(o.TokenURL))
		secret(a.child("oauth2ClientCredentials", "clientId"), o.ClientID)
		secret(a.child("oauth2ClientCredentials", "clientSecret"), o.ClientSecret)
	case auth.Type == AuthSessionLogin && auth.SessionLogin != nil:
		s := auth.SessionLogin
		sa := a.child("sessionLogin")
		for _, k := range sortedKeys(s.Request.Body) {
			secret(sa.child("request", "body", k), s.Request.Body[k])
		}
		if (s.Apply.Header == "") == (s.Apply.Cookie == "") {
			c.errorf(sa.child("apply"), "apply must set exactly one of header or cookie")
		}
		if s.Apply.Format != "" && s.Apply.Header == "" {
			c.errorf(sa.child("apply", "format"), "format is only allowed with header")
		}
	}
}

// checkSecretRef checks that {secret: name} refers to a declared secret.
func (c *checker) checkSecretRef(a at, v Value, p *Provider) {
	if v.Secret == "" || p == nil {
		return
	}
	for _, s := range p.Spec.Secrets {
		if string(s.Name) == v.Secret {
			return
		}
	}
	c.errorf(a, "secret %q is not declared in provider %q", v.Secret, p.Metadata.Name)
}

func (c *checker) checkPagination(a at, p *Pagination) {
	set := map[string]bool{
		"offset":     p.Offset != nil,
		"page":       p.Page != nil,
		"linkHeader": p.LinkHeader != nil,
		"cursor":     p.Cursor != nil,
		"expression": p.Expression != nil,
	}
	for _, block := range sortedKeys(set) {
		if set[block] && paginationBlocks[p.Type] != block {
			c.errorf(a.child(block), "%s is not allowed with pagination type %s", block, p.Type)
		}
	}
	if p.Cursor != nil && (p.Cursor.Param == "") == (p.Cursor.Header == "") {
		c.errorf(a.child("cursor"), "cursor must set exactly one of param or header")
	}
}

func (c *checker) checkTemplate(t *JobTemplate) {
	base := loc(t.Source, "spec")
	p, ok := c.providers[string(t.Spec.Provider)]
	if !ok {
		c.errorf(base.child("provider"), "unknown provider %q", t.Spec.Provider)
	}
	if t.Spec.ForEach != nil && t.Spec.ForEach.Job != "" {
		c.errorf(base.child("forEach", "job"), "forEach.job is only allowed in inline jobs; use forEach.template")
	}
	c.checkDefinition(base, &t.Spec.JobDefinition, p)
}

// checkDefinition checks the fields shared by templates and inline jobs.
// p may be nil when the provider is unknown (already reported).
func (c *checker) checkDefinition(base at, d *JobDefinition, p *Provider) {
	if d.Pagination != nil {
		c.checkPagination(base.child("pagination"), d.Pagination)
	}
	fe := d.ForEach
	if fe != nil {
		fa := base.child("forEach")
		if (fe.Template == "") == (fe.Job == "") {
			c.errorf(fa, "forEach must set exactly one of template or job")
		}
		if fe.Template != "" {
			parent, ok := c.templates[string(fe.Template)]
			switch {
			case !ok:
				c.errorf(fa.child("template"), "unknown job template %q", fe.Template)
			case p != nil && parent.Spec.Provider != p.Metadata.Name:
				c.errorf(fa.child("template"), "parent template %q belongs to provider %q, not %q",
					fe.Template, parent.Spec.Provider, p.Metadata.Name)
			case !roleOrDefault(parent.Spec.Role).producesInventory():
				c.errorf(fa.child("template"), "parent template %q must have role inventory or both", fe.Template)
			}
		}
	}
	if r := d.Request; r != nil {
		ra := base.child("request")
		if r.Method == http.MethodPost && !r.ReadOnly {
			c.errorf(ra.child("method"), "POST requests must be declared readOnly: true (Tapyr never writes to providers)")
		}
		if r.Method != http.MethodPost && r.Body != nil {
			c.errorf(ra.child("body"), "body is only allowed for POST requests")
		}
		if r.Idempotent && r.Method != http.MethodPost {
			c.errorf(ra.child("idempotent"), "idempotent is only meaningful for POST requests")
		}
		c.checkPathVars(ra.child("path"), r.Path, c.discoveryVars(d, map[*JobDefinition]bool{}), p)
		for _, k := range sortedKeys(r.Query) {
			c.checkSecretRef(ra.child("query", k), r.Query[k], p)
		}
		for _, k := range sortedKeys(r.Body) {
			c.checkSecretRef(ra.child("body", k), r.Body[k], p)
		}
	}
	role := roleOrDefault(d.Role)
	if e := d.Extract; e != nil {
		ea := base.child("extract")
		if role.producesInventory() && e.Inventory == nil {
			c.errorf(ea, "role %s requires extract.inventory", role)
		}
		if !role.producesInventory() && e.Inventory != nil {
			c.errorf(ea.child("inventory"), "extract.inventory requires role inventory or both")
		}
		c.checkMetrics(ea.child("metrics"), e.Metrics)
		c.checkMetrics(ea.child("post"), e.Post)
		for i, m := range e.Info {
			c.declareMetric(ea.child("info", strconv.Itoa(i), "name"), m.Name, MetricGauge)
		}
	} else if role.producesInventory() {
		c.errorf(base, "role %s requires extract.inventory", role)
	}
}

func roleOrDefault(r JobRole) JobRole {
	if r == "" {
		return RoleMetrics
	}
	return r
}

func (c *checker) checkMetrics(a at, metrics []Metric) {
	for i, m := range metrics {
		ma := a.child(strconv.Itoa(i))
		typ := m.Type
		if typ == "" {
			typ = MetricGauge
		}
		if m.Accumulate && typ != MetricCounter {
			c.errorf(ma.child("accumulate"), "accumulate requires type counter")
		}
		if m.WindowField != "" && !m.Accumulate {
			c.errorf(ma.child("windowField"), "windowField is only allowed with accumulate: true")
		}
		c.declareMetric(ma.child("name"), m.Name, typ)
	}
}

// declareMetric enforces a single type per metric name across all jobs.
func (c *checker) declareMetric(a at, name string, typ MetricType) {
	prev, ok := c.metricTypes[name]
	if !ok {
		c.metricTypes[name] = metricDecl{typ: typ, src: a.src, at: a.tokens}
		return
	}
	if prev.typ != typ {
		c.errorf(a, "metric %q is declared as %s here and as %s at %s", name, typ, prev.typ,
			c.posOf(loc(prev.src, prev.at...)))
	}
}

// discoveryVars returns the variables set by the fan-outs of a definition
// and of its ancestors (children inherit the variables of their parent).
func (c *checker) discoveryVars(d *JobDefinition, seen map[*JobDefinition]bool) map[string]bool {
	out := map[string]bool{}
	if d == nil || d.ForEach == nil || seen[d] {
		return out
	}
	seen[d] = true
	fe := d.ForEach
	for k := range fe.Vars {
		out[k] = true
	}
	var parent *JobDefinition
	switch {
	case fe.Template != "":
		if t, ok := c.templates[string(fe.Template)]; ok {
			parent = &t.Spec.JobDefinition
		}
	case fe.Job != "":
		if j, ok := c.jobs[string(fe.Job)]; ok {
			parent = &j.Spec.JobDefinition
			if t, ok := c.templates[string(j.Spec.Template)]; ok {
				parent = &t.Spec.JobDefinition
			}
		}
	}
	for k := range c.discoveryVars(parent, seen) {
		out[k] = true
	}
	return out
}

// checkPathVars checks that every {var} of a path is provided by the
// discovery variables or by a provider param.
func (c *checker) checkPathVars(a at, path string, discovery map[string]bool, p *Provider) {
	for _, v := range pathVars(path) {
		if !identifierRe.MatchString(v) {
			c.errorf(a, "invalid path variable {%s}: %s", v, patternHints[identifierPattern])
			continue
		}
		if discovery[v] {
			continue
		}
		if p != nil && slices.ContainsFunc(p.Spec.Params, func(ps ParamSpec) bool { return string(ps.Name) == v }) {
			continue
		}
		if p == nil {
			continue // unknown provider, already reported
		}
		c.errorf(a, "path variable {%s} is not provided by the forEach.vars of the job or of its parents, or by a param of provider %q", v, p.Metadata.Name)
	}
}

func (c *checker) checkAccount(acc *Account) {
	base := loc(acc.Source, "spec")
	p, ok := c.providers[string(acc.Spec.Provider)]
	if !ok {
		c.errorf(base.child("provider"), "unknown provider %q", acc.Spec.Provider)
		return
	}
	targetNames := map[Name]bool{}
	for i, t := range acc.Spec.Targets {
		if targetNames[t.Name] {
			c.errorf(base.child("targets", strconv.Itoa(i), "name"), "duplicate target name %q", t.Name)
		}
		targetNames[t.Name] = true
	}
	type field struct {
		kind     string // params or secrets
		declared []ParamSpec
		account  []string
		target   func(Target) []string
	}
	for _, f := range []field{
		{"params", p.Spec.Params, sortedKeys(acc.Spec.Params), func(t Target) []string { return sortedKeys(t.Params) }},
		{"secrets", p.Spec.Secrets, sortedKeys(acc.Spec.Secrets), func(t Target) []string { return sortedKeys(t.Secrets) }},
	} {
		declared := map[string]bool{}
		for _, d := range f.declared {
			declared[string(d.Name)] = true
		}
		singular := strings.TrimSuffix(f.kind, "s")
		for _, k := range f.account {
			if !declared[k] {
				c.errorf(base.child(f.kind, k), "%s %q is not declared by provider %q", singular, k, p.Metadata.Name)
			}
		}
		for i, t := range acc.Spec.Targets {
			for _, k := range f.target(t) {
				if !declared[k] {
					c.errorf(base.child("targets", strconv.Itoa(i), f.kind, k), "%s %q is not declared by provider %q", singular, k, p.Metadata.Name)
				}
			}
		}
		for _, d := range f.declared {
			if !d.Required || slices.Contains(f.account, string(d.Name)) {
				continue
			}
			if len(acc.Spec.Targets) == 0 {
				c.errorf(base, "missing required %s %q (declared by provider %q)", singular, d.Name, p.Metadata.Name)
				continue
			}
			for i, t := range acc.Spec.Targets {
				if !slices.Contains(f.target(t), string(d.Name)) {
					c.errorf(base.child("targets", strconv.Itoa(i)), "missing required %s %q for target %q (declared by provider %q)",
						singular, d.Name, t.Name, p.Metadata.Name)
				}
			}
		}
	}
}

func (c *checker) checkJob(j *Job) {
	base := loc(j.Source, "spec")
	spec := &j.Spec
	acc, accOK := c.accounts[string(spec.Account)]
	if !accOK {
		c.errorf(base.child("account"), "unknown account %q", spec.Account)
	}
	var p *Provider
	if accOK {
		p = c.providers[string(acc.Spec.Provider)]
	}
	for i, o := range spec.Outputs {
		if _, ok := c.outputs[string(o)]; !ok {
			c.errorf(base.child("outputs", strconv.Itoa(i)), "unknown output %q", o)
		}
	}
	if spec.Interval != 0 && spec.Interval.Std() <= 0 {
		c.errorf(base.child("interval"), "interval must be greater than zero")
	}

	if spec.Template != "" {
		for _, f := range spec.definitionFields() {
			c.errorf(base.child(f), "%s is not allowed in a job that uses a template", f)
		}
		t, ok := c.templates[string(spec.Template)]
		if !ok {
			c.errorf(base.child("template"), "unknown job template %q", spec.Template)
			return
		}
		if p != nil && t.Spec.Provider != p.Metadata.Name {
			c.errorf(base.child("template"), "template %q belongs to provider %q, but account %q uses provider %q",
				spec.Template, t.Spec.Provider, spec.Account, p.Metadata.Name)
		}
		return
	}

	if spec.Request == nil {
		c.errorf(base, "a job without template requires request")
	}
	if spec.Interval == 0 {
		c.errorf(base, "a job without template requires interval")
	}
	if fe := spec.ForEach; fe != nil && fe.Job != "" {
		parent, ok := c.jobs[string(fe.Job)]
		switch {
		case !ok:
			c.errorf(base.child("forEach", "job"), "unknown job %q", fe.Job)
		case parent.Spec.Account != spec.Account:
			c.errorf(base.child("forEach", "job"), "parent job %q collects for account %q, not %q",
				fe.Job, parent.Spec.Account, spec.Account)
		case !c.jobRole(parent).producesInventory():
			c.errorf(base.child("forEach", "job"), "parent job %q must have role inventory or both", fe.Job)
		}
	}
	c.checkDefinition(base, &spec.JobDefinition, p)
}

// jobRole returns the effective role of a job.
func (c *checker) jobRole(j *Job) JobRole {
	if j.Spec.Template != "" {
		if t, ok := c.templates[string(j.Spec.Template)]; ok {
			return roleOrDefault(t.Spec.Role)
		}
	}
	return roleOrDefault(j.Spec.Role)
}

func (c *checker) checkOutputs() {
	var prometheus *Output
	for _, o := range c.cfg.Outputs {
		base := loc(o.Source, "spec")
		set := map[string]bool{
			"prometheus":  o.Spec.Prometheus != nil,
			"remoteWrite": o.Spec.RemoteWrite != nil,
			"otlp":        o.Spec.OTLP != nil,
		}
		for _, block := range sortedKeys(set) {
			if set[block] && outputBlocks[o.Spec.Type] != block {
				c.errorf(base.child(block), "%s is not allowed with output type %s", block, o.Spec.Type)
			}
		}
		c.checkProcessing(o)
		if o.Spec.Type != OutputPrometheus {
			continue
		}
		if o.Spec.Queue != nil {
			c.errorf(base.child("queue"), "queue is not allowed for prometheus outputs (pull exposition)")
		}
		if prometheus != nil {
			c.errorf(base.child("type"), "at most one prometheus output is allowed (another one is %q at %s)",
				prometheus.Metadata.Name, c.posOf(loc(prometheus.Source, "spec", "type")))
			continue
		}
		prometheus = o
	}
}

// checkProcessing checks the processing rules of an output (ADR 0012).
func (c *checker) checkProcessing(o *Output) {
	var metrics []string
	for i, r := range o.Spec.Processing {
		a := loc(o.Source, "spec", "processing", strconv.Itoa(i))
		switch r.Action {
		case ProcessingKeep, ProcessingDrop:
			if len(r.SourceLabels) == 0 {
				c.errorf(a, "%s requires sourceLabels", r.Action)
			}
		case ProcessingLabelKeep, ProcessingLabelDrop:
			if len(r.SourceLabels) > 0 {
				c.errorf(a.child("sourceLabels"), "sourceLabels is not allowed with %s", r.Action)
			}
			if r.Separator != nil {
				c.errorf(a.child("separator"), "separator is not allowed with %s", r.Action)
			}
			if r.Regex == "" {
				c.errorf(a, "%s requires regex", r.Action)
				continue
			}
		}
		re, err := CompileProcessingRegex(r.Regex)
		if err != nil {
			c.errorf(a.child("regex"), "invalid regular expression: %v", err)
			continue
		}
		switch {
		case r.Action == ProcessingLabelKeep && !re.MatchString("__name__"):
			c.errorf(a.child("regex"), "labelkeep must keep __name__ (the metric name): the regex does not match it")
		case r.Action == ProcessingLabelDrop && re.MatchString("__name__"):
			c.errorf(a.child("regex"), "labeldrop must not remove __name__ (the metric name): the regex matches it")
		case (r.Action == ProcessingKeep || r.Action == ProcessingDrop) && slices.Equal(r.SourceLabels, []string{"__name__"}):
			if metrics == nil {
				metrics = c.outputMetrics(string(o.Metadata.Name))
			}
			if len(metrics) > 0 && !slices.ContainsFunc(metrics, re.MatchString) {
				c.warnf(a.child("regex"), "%s matches no metric of the jobs sent to output %q", r.Action, o.Metadata.Name)
			}
		}
	}
}

// outputMetrics returns the metric names that the jobs sent to an output
// may produce, sorted: the metrics of every template of the providers of
// those jobs (their implicit parents included) and of their inline
// definitions.
func (c *checker) outputMetrics(output string) []string {
	names := map[string]bool{}
	add := func(e *Extract) {
		if e == nil {
			return
		}
		for _, m := range e.Metrics {
			names[m.Name] = true
		}
		for _, m := range e.Info {
			names[m.Name] = true
		}
		for _, m := range e.Post {
			names[m.Name] = true
		}
	}
	for _, j := range c.cfg.Jobs {
		if len(j.Spec.Outputs) > 0 && !slices.ContainsFunc(j.Spec.Outputs, func(o Name) bool { return string(o) == output }) {
			continue
		}
		add(j.Spec.Extract)
		acc, ok := c.accounts[string(j.Spec.Account)]
		if !ok {
			continue
		}
		for _, t := range c.cfg.JobTemplates {
			if t.Spec.Provider == acc.Spec.Provider {
				add(t.Spec.Extract)
			}
		}
	}
	return sortedKeys(names)
}

// CompileProcessingRegex compiles the regex of a processing rule, fully
// anchored; an empty regex is (.*).
func CompileProcessingRegex(re string) (*regexp.Regexp, error) {
	if re == "" {
		re = "(.*)"
	}
	return regexp.Compile("^(?:" + re + ")$")
}

func (c *checker) checkSettings(s *Settings) {
	base := loc(s.Source, "spec")
	spec := &s.Spec
	for i, o := range spec.SelfMetrics.Outputs {
		if _, ok := c.outputs[string(o)]; !ok {
			c.errorf(base.child("selfMetrics", "outputs", strconv.Itoa(i)), "unknown output %q", o)
		}
	}
	percentRange := func(a at, p *Percent, maxFraction float64) {
		if p != nil && p.Fraction() > maxFraction {
			c.errorf(a, "must be between 0%% and %g%%", maxFraction*100)
		}
	}
	percentRange(base.child("collection", "jitter"), spec.Collection.Jitter, 0.5)
	percentRange(base.child("budget", "retryReserve"), spec.Budget.RetryReserve, 0.5)
	if r := spec.Retry; r.InitialBackoff != 0 && r.MaxBackoff != 0 && r.MaxBackoff < r.InitialBackoff {
		c.errorf(base.child("retry", "maxBackoff"), "maxBackoff must be greater than or equal to initialBackoff")
	}
	if cb := spec.CircuitBreaker; cb.Cooldown != 0 && cb.MaxCooldown != 0 && cb.MaxCooldown < cb.Cooldown {
		c.errorf(base.child("circuitBreaker", "maxCooldown"), "maxCooldown must be greater than or equal to cooldown")
	}
}

// checkDiscovery rejects cycles and chains deeper than maxDiscoveryDepth.
// Nodes are templates and jobs; edges go from a child to its parent.
func (c *checker) checkDiscovery() {
	type node struct {
		kind Kind
		name string
	}
	parent := func(n node) (node, at, bool) {
		var fe *ForEach
		var a at
		switch n.kind {
		case KindJobTemplate:
			t := c.templates[n.name]
			fe, a = t.Spec.ForEach, loc(t.Source, "spec", "forEach")
		case KindJob:
			j := c.jobs[n.name]
			if j.Spec.Template != "" {
				if _, ok := c.templates[string(j.Spec.Template)]; !ok {
					return node{}, at{}, false
				}
				return node{KindJobTemplate, string(j.Spec.Template)}, loc(j.Source, "spec", "template"), true
			}
			fe, a = j.Spec.ForEach, loc(j.Source, "spec", "forEach")
		}
		if fe == nil {
			return node{}, at{}, false
		}
		if fe.Job != "" {
			if _, ok := c.jobs[string(fe.Job)]; ok {
				return node{KindJob, string(fe.Job)}, a.child("job"), true
			}
		} else if _, ok := c.templates[string(fe.Template)]; ok {
			return node{KindJobTemplate, string(fe.Template)}, a.child("template"), true
		}
		return node{}, at{}, false
	}
	label := func(n node) string { return fmt.Sprintf("%s %q", n.kind, n.name) }
	check := func(start node, a at) {
		// Walk up the chain; depth counts fan-out levels (a job using a
		// template shares the level of its template).
		// edges[k] is the reference from path[k] to its parent path[k+1].
		path := []node{start}
		var edges []at
		depth := 1
		for n := start; ; {
			p, edge, ok := parent(n)
			if !ok {
				break
			}
			edges = append(edges, edge)
			if i := slices.Index(path, p); i >= 0 {
				// Report the cycle once, from its smallest member, so every
				// entry point yields the same diagnostic.
				cycle, cycleEdges := path[i:], edges[i:]
				first := 0
				for k := range cycle {
					if label(cycle[k]) < label(cycle[first]) {
						first = k
					}
				}
				names := make([]string, 0, len(cycle)+1)
				for k := range len(cycle) + 1 {
					names = append(names, label(cycle[(first+k)%len(cycle)]))
				}
				c.errorf(cycleEdges[first], "discovery cycle: %s", strings.Join(names, " -> "))
				return
			}
			if n.kind != KindJob || c.jobs[n.name].Spec.Template == "" {
				depth++
			}
			path = append(path, p)
			n = p
		}
		if depth > maxDiscoveryDepth {
			c.errorf(a, "discovery chain is %d levels deep (maximum %d)", depth, maxDiscoveryDepth)
		}
	}
	for _, t := range c.cfg.JobTemplates {
		if t.Spec.ForEach != nil {
			check(node{KindJobTemplate, string(t.Metadata.Name)}, loc(t.Source, "spec", "forEach"))
		}
	}
	for _, j := range c.cfg.Jobs {
		if j.Spec.Template == "" && j.Spec.ForEach != nil {
			check(node{KindJob, string(j.Metadata.Name)}, loc(j.Source, "spec", "forEach"))
		}
	}
}
