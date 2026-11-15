// Package runtime compiles a validated configuration into the immutable
// runtime graph: CEL programs compiled, template fields evaluated per
// account, effective request, pagination, auth and labels resolved per job
// (see docs/spec/02-architecture.md, "Runtime compiler").
package runtime

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/kentrow/tapyr/internal/auth"
	"github.com/kentrow/tapyr/internal/config"
	"github.com/kentrow/tapyr/internal/mapping"
	"github.com/kentrow/tapyr/internal/secret"
)

// Options configures Compile.
type Options struct {
	// Secrets resolves account secrets. When nil, secrets are not resolved
	// and jobs get no authenticator (tapyr validate).
	Secrets *config.SecretSource
	// CredentialSalt keys the credential hash (stored in the state store so
	// that ledger keys survive restarts).
	CredentialSalt []byte
	// Auth shares the token sessions of the accounts. When nil, a manager
	// without token storage is used.
	Auth *auth.Manager
}

// Graph is the compiled configuration.
type Graph struct {
	Settings *config.SettingsSpec
	Jobs     []*Job
	// NotCollected are the jobs whose metrics no output keeps, and the
	// parents whose inventory no collected job takes (ADR 0013): compiled,
	// but neither scheduled nor planned.
	NotCollected []*Job
	Outputs      []*config.Output
	// OutputCredentials are the resolved headers and basic auth of the
	// push outputs, by output name (when secrets are resolved).
	OutputCredentials map[string]*OutputCredentials
	// TLS holds the TLS configuration of the accounts that declare one, by
	// account name.
	TLS map[string]*AccountTLS
}

// OutputCredentials are the resolved credentials of a push output.
type OutputCredentials struct {
	// Headers are sent with every request; values may be secrets.
	Headers map[string]secret.Value
	// Username and Password are the basic auth credentials, zero when
	// none.
	Username, Password secret.Value
}

// Job is a runnable job instance: one per job and target; jobs with a
// fan-out (ForEach) are the model of their child instances (see Child).
type Job struct {
	Name string
	// Instance identifies the instance: the job name, the target and the
	// discovery variables (devices[target=north-01,orgId=42]).
	Instance string
	Account  string
	// Template is the job template of the job, empty for an inline job.
	Template string
	// Target is the target of a multi-target account, or empty.
	Target   string
	Provider string
	// Implicit marks a parent instantiated for a forEach.template that no
	// job of the account uses.
	Implicit bool
	Role     config.JobRole
	Interval time.Duration
	Priority config.Priority
	// Outputs receiving the samples; empty means all outputs.
	Outputs []string
	BaseURL *url.URL
	Request Request
	// Pagination is the effective pagination (job, template or provider).
	Pagination Pagination
	Extract    *mapping.Extract
	// Auth is nil when secrets were not resolved.
	Auth auth.Authenticator
	// Credential is the keyed hash of the account secrets (scope dimension
	// credential), empty when secrets were not resolved.
	Credential string
	Errors     *config.ErrorSemantics
	// RedirectHosts are the other hosts the requests may be redirected to
	// (provider redirects.hosts).
	RedirectHosts []string
	// Vars are the request variables: account params, overridden by target
	// params, overridden by discovery variables.
	Vars map[string]string
	// DiscoveryVars are the variables set by fan-outs (part of the instance key).
	DiscoveryVars map[string]string
	// AccountInfo is the CEL account variable: name, labels and params.
	AccountInfo map[string]any
	// TargetInfo is the CEL target variable: name, labels and params.
	TargetInfo map[string]any
	// StaleAfter withdraws the series of an instance without a successful
	// run for this long (default 3x the effective interval, computed by the
	// collector when 0).
	StaleAfter time.Duration
	// InventoryTTL is how long the inventory items of the job stay valid
	// (default 3x the effective interval, computed by the collector when 0).
	InventoryTTL time.Duration
	// Labels added to every sample, in increasing precedence.
	Labels []map[string]string
	Limits mapping.Limits
	// PagesEstimate is the estimate.pagesPerRun hint (0 when absent).
	PagesEstimate int
	// ForEach describes the discovery fan-out of the job, or nil.
	ForEach *ForEach
	// NotCollected is the reason why the job is not collected (ADR 0013),
	// empty for a collected job.
	NotCollected string
}

// ForEach describes a fan-out.
type ForEach struct {
	// Parent is the parent template or job name as written (plan hints).
	Parent string
	// ParentJob is the name of the runtime job of the parent.
	ParentJob string
	// Vars are the names of the variables set per instance.
	Vars         []string
	programs     map[string]*mapping.Program
	filter       *mapping.Program
	MaxInstances int
}

// Child returns the child instance of a fan-out job for one inventory item
// of a parent instance, or false when forEach.filter excludes the item.
func (j *Job) Child(parent *Job, item mapping.InventoryItem, now time.Time) (*Job, bool, error) {
	fe := j.ForEach
	act := &mapping.Activation{Parent: item.Fields, Account: j.AccountInfo, Target: j.TargetInfo, Now: now}
	if fe.filter != nil {
		v, err := fe.filter.Eval(act)
		var keep bool
		if err == nil {
			keep, err = mapping.ToBool(v)
		}
		if err != nil {
			return nil, false, fmt.Errorf("forEach.filter: %w", err)
		}
		if !keep {
			return nil, false, nil
		}
	}
	child := *j
	child.ForEach = nil
	child.DiscoveryVars = map[string]string{}
	for k, v := range parent.DiscoveryVars {
		child.DiscoveryVars[k] = v
	}
	for _, name := range fe.Vars {
		v, err := fe.programs[name].Eval(act)
		var s string
		if err == nil {
			s, err = mapping.ToLabel(v)
		}
		if err != nil {
			return nil, false, fmt.Errorf("forEach.vars.%s: %w", name, err)
		}
		child.DiscoveryVars[name] = s
	}
	child.Vars = map[string]string{}
	for k, v := range j.Vars {
		child.Vars[k] = v
	}
	for k, v := range child.DiscoveryVars {
		child.Vars[k] = v
	}
	child.Instance = instanceKey(j.Name, j.Target, child.DiscoveryVars)
	return &child, true, nil
}

// instanceKey formats the identity of an instance.
func instanceKey(name, target string, vars map[string]string) string {
	var parts []string
	if target != "" {
		parts = append(parts, "target="+target)
	}
	for _, k := range sortedKeys(vars) {
		parts = append(parts, k+"="+vars[k])
	}
	if len(parts) == 0 {
		return name
	}
	return name + "[" + strings.Join(parts, ",") + "]"
}

// Request is the effective request of a job.
type Request struct {
	Method string
	// Path is relative to the base URL, with {var} placeholders.
	Path       string
	Query      []Param
	Headers    map[string]string
	Body       []Param
	Timeout    time.Duration
	Idempotent bool
}

// Param is a query or body value: a literal, a secret or a CEL expression.
type Param struct {
	Name    string
	Literal string
	Secret  secret.Value
	Expr    *mapping.Program
}

// Pagination is the compiled effective pagination of a job.
type Pagination struct {
	Type       config.PaginationType
	MaxPages   int
	Offset     *config.OffsetPagination
	Page       *config.PagePagination
	LinkHeader *config.LinkHeaderPagination
	Cursor     *config.CursorPagination
	// Stop and Total are the offset or page pagination expressions, or nil.
	Stop  *mapping.Program
	Total *mapping.Program
	// Next is the cursor expression (cursor) or the next-request expression
	// (expression).
	Next *mapping.Program
}

// compiledDefinition is a compiled template or inline job definition.
type compiledDefinition struct {
	extract    *mapping.Extract
	pagination *Pagination // nil: use the provider pagination
	query      map[string]*mapping.Program
	body       map[string]*mapping.Program
	forEach    map[string]*mapping.Program
	filter     *mapping.Program
}

type compiler struct {
	cfg       *config.Config
	opts      Options
	env       *mapping.Env
	diags     config.Diagnostics
	providers map[config.Name]*config.Provider
	templates map[config.Name]*config.JobTemplate
	accounts  map[config.Name]*config.Account
	defs      map[config.Name]*compiledDefinition
	provPages map[config.Name]*Pagination
	logins    map[config.Name]*auth.LoginPrograms
	accData   map[config.Name]*accountData
	graph     *Graph
	// parents maps account and template to the job instantiating it.
	parents map[[2]config.Name]string
	// implicit marks the implicit parent jobs.
	implicit map[string]bool
}

// accountData holds what is rendered and resolved per account and target.
type accountData struct {
	targets []*targetData
	ok      bool
}

type targetData struct {
	name       string
	params     map[string]string
	labels     map[string]string
	baseURL    *url.URL
	headers    map[string]string
	secrets    auth.Secrets
	credential string
	info       map[string]any
}

// Compile builds the runtime graph. Errors are returned as Diagnostics.
func Compile(cfg *config.Config, opts Options) (*Graph, error) {
	settings := &cfg.Settings.Spec
	env, err := mapping.NewEnv(settings.Mapping.MaxCost)
	if err != nil {
		return nil, err
	}
	if opts.Auth == nil {
		return nil, errors.New("runtime: Options.Auth is required (the token manager with the auth plugins)")
	}
	c := &compiler{
		cfg: cfg, opts: opts, env: env,
		providers: map[config.Name]*config.Provider{},
		templates: map[config.Name]*config.JobTemplate{},
		accounts:  map[config.Name]*config.Account{},
		defs:      map[config.Name]*compiledDefinition{},
		provPages: map[config.Name]*Pagination{},
		logins:    map[config.Name]*auth.LoginPrograms{},
		accData:   map[config.Name]*accountData{},
		graph:     &Graph{Settings: settings, Outputs: cfg.Outputs},
		parents:   map[[2]config.Name]string{},
		implicit:  map[string]bool{},
	}
	for _, p := range cfg.Providers {
		c.providers[p.Metadata.Name] = p
		c.compileProvider(p)
	}
	for _, t := range cfg.JobTemplates {
		c.templates[t.Metadata.Name] = t
		c.defs[t.Metadata.Name] = c.compileDefinition(t.Source, &t.Spec.JobDefinition)
	}
	for _, a := range cfg.Accounts {
		c.accounts[a.Metadata.Name] = a
		c.accData[a.Metadata.Name] = c.compileAccount(a)
		c.compileTLS(a)
	}
	for _, j := range c.withImplicitParents(cfg.Jobs) {
		c.compileJob(j)
	}
	for _, o := range cfg.Outputs {
		c.compileOutput(o)
	}
	if len(c.diags) > 0 {
		return nil, c.diags.Sorted()
	}
	c.splitCollected()
	return c.graph, nil
}

// compileOutput checks the endpoint of a push output and resolves its
// credentials.
func (c *compiler) compileOutput(o *config.Output) {
	if otlp := o.Spec.OTLP; otlp != nil {
		if u, err := url.Parse(otlp.Endpoint); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			c.errorf(o.Source, []string{"spec", "otlp", "endpoint"}, "endpoint %q must be an http or https URL", otlp.Endpoint)
		}
	}
	if c.opts.Secrets == nil {
		return
	}
	var headers map[string]config.StringOrSecret
	var basic *config.BasicAuthSecrets
	block := ""
	switch {
	case o.Spec.RemoteWrite != nil:
		headers, basic, block = o.Spec.RemoteWrite.Headers, o.Spec.RemoteWrite.BasicAuth, "remoteWrite"
	case o.Spec.OTLP != nil:
		headers, block = o.Spec.OTLP.Headers, "otlp"
	default:
		return
	}
	creds := &OutputCredentials{Headers: map[string]secret.Value{}}
	resolve := func(ref config.SecretRef, tokens ...string) secret.Value {
		v, err := secret.Resolve(ref, *c.opts.Secrets)
		if err != nil {
			c.errorf(o.Source, append([]string{"spec", block}, tokens...), "%v", err)
		}
		return v
	}
	for _, name := range sortedKeys(headers) {
		h := headers[name]
		if h.Secret == nil {
			creds.Headers[name] = secret.New(h.Literal)
			continue
		}
		creds.Headers[name] = resolve(*h.Secret, "headers", name)
	}
	if basic != nil {
		creds.Username = resolve(basic.Username, "basicAuth", "username")
		creds.Password = resolve(basic.Password, "basicAuth", "password")
	}
	if c.graph.OutputCredentials == nil {
		c.graph.OutputCredentials = map[string]*OutputCredentials{}
	}
	c.graph.OutputCredentials[string(o.Metadata.Name)] = creds
}

func (c *compiler) errorf(src *config.Source, tokens []string, format string, args ...any) {
	c.diags = append(c.diags, src.Diagnostic(fmt.Sprintf(format, args...), tokens...))
}

func (c *compiler) compile(src *config.Source, ctx mapping.Context, expr config.Expr, out mapping.Output, tokens ...string) *mapping.Program {
	if expr == "" {
		return nil
	}
	p, err := c.env.Compile(ctx, string(expr), out)
	if err != nil {
		c.errorf(src, tokens, "%v", err)
	}
	return p
}

func (c *compiler) compileProvider(p *config.Provider) {
	spec := &p.Spec
	if spec.Pagination != nil {
		c.provPages[p.Metadata.Name] = c.compilePagination(p.Source, spec.Pagination, "spec", "pagination")
	}
	if s := spec.Auth.SessionLogin; s != nil {
		lp := &auth.LoginPrograms{
			Token:     c.compile(p.Source, mapping.ContextLogin, s.Token, mapping.OutputString, "spec", "auth", "sessionLogin", "token"),
			ExpiresIn: c.compile(p.Source, mapping.ContextLogin, s.ExpiresIn, mapping.OutputDuration, "spec", "auth", "sessionLogin", "expiresIn"),
			Body:      map[string]*mapping.Program{},
		}
		for _, k := range sortedKeys(s.Request.Body) {
			lp.Body[k] = c.compile(p.Source, mapping.ContextRequest, config.Expr(s.Request.Body[k].Expr), mapping.OutputString,
				"spec", "auth", "sessionLogin", "request", "body", k)
		}
		c.logins[p.Metadata.Name] = lp
	}
	if pa := spec.Auth.Plugin; pa != nil {
		refs, err := c.opts.Auth.ValidatePlugin(pa)
		if err != nil {
			c.errorf(p.Source, []string{"spec", "auth", "plugin"}, "%v", err)
		}
		for _, name := range refs {
			if !slices.ContainsFunc(spec.Secrets, func(ps config.ParamSpec) bool { return string(ps.Name) == name }) {
				c.errorf(p.Source, []string{"spec", "auth", "plugin", "config"}, "secret %q is not declared in spec.secrets", name)
			}
		}
	}
}

func (c *compiler) compilePagination(src *config.Source, p *config.Pagination, tokens ...string) *Pagination {
	at := func(more ...string) []string { return append(slices.Clip(tokens), more...) }
	out := &Pagination{Type: p.Type, MaxPages: p.MaxPages, Offset: p.Offset, Page: p.Page, LinkHeader: p.LinkHeader, Cursor: p.Cursor}
	switch {
	case p.Offset != nil:
		out.Stop = c.compile(src, mapping.ContextPage, p.Offset.Stop, mapping.OutputBool, at("offset", "stop")...)
		out.Total = c.compile(src, mapping.ContextPage, p.Offset.Total, mapping.OutputNumber, at("offset", "total")...)
	case p.Page != nil:
		out.Stop = c.compile(src, mapping.ContextPage, p.Page.Stop, mapping.OutputBool, at("page", "stop")...)
		out.Total = c.compile(src, mapping.ContextPage, p.Page.Total, mapping.OutputNumber, at("page", "total")...)
	case p.Cursor != nil:
		out.Next = c.compile(src, mapping.ContextPage, p.Cursor.Cursor, mapping.OutputString, at("cursor", "cursor")...)
	case p.Expression != nil:
		out.Next = c.compile(src, mapping.ContextPage, p.Expression.Next, mapping.OutputMap, at("expression", "next")...)
	}
	return out
}

func (c *compiler) compileDefinition(src *config.Source, d *config.JobDefinition) *compiledDefinition {
	out := &compiledDefinition{query: map[string]*mapping.Program{}, body: map[string]*mapping.Program{}}
	x, errs := c.env.CompileExtract(d.Extract)
	for _, fe := range errs {
		c.errorf(src, append([]string{"spec", "extract"}, fe.Path...), "%v", fe.Err)
	}
	out.extract = x
	if d.Pagination != nil {
		out.pagination = c.compilePagination(src, d.Pagination, "spec", "pagination")
	}
	if fe := d.ForEach; fe != nil {
		out.forEach = map[string]*mapping.Program{}
		for _, k := range sortedKeys(fe.Vars) {
			out.forEach[k] = c.compile(src, mapping.ContextParent, fe.Vars[k], mapping.OutputString, "spec", "forEach", "vars", k)
		}
		out.filter = c.compile(src, mapping.ContextParent, fe.Filter, mapping.OutputBool, "spec", "forEach", "filter")
	}
	if r := d.Request; r != nil {
		for _, k := range sortedKeys(r.Query) {
			if p := c.compile(src, mapping.ContextRequest, config.Expr(r.Query[k].Expr), mapping.OutputString, "spec", "request", "query", k); p != nil {
				out.query[k] = p
			}
		}
		for _, k := range sortedKeys(r.Body) {
			if p := c.compile(src, mapping.ContextRequest, config.Expr(r.Body[k].Expr), mapping.OutputString, "spec", "request", "body", k); p != nil {
				out.body[k] = p
			}
		}
	}
	return out
}

// compileAccount evaluates the provider template fields with the params of
// every target (account params overridden by target params) and resolves
// the secrets (target secrets override account secrets).
func (c *compiler) compileAccount(a *config.Account) *accountData {
	p, ok := c.providers[a.Spec.Provider]
	if !ok {
		return &accountData{}
	}
	data := &accountData{ok: true}
	targets := a.Spec.Targets
	if len(targets) == 0 {
		targets = []config.Target{{}}
	}
	for i := range targets {
		t := &targets[i]
		tokens := []string{"spec", "provider"}
		if t.Name != "" {
			tokens = []string{"spec", "targets", strconv.Itoa(i)}
		}
		td := &targetData{name: string(t.Name), params: map[string]string{}, headers: map[string]string{}, labels: t.Labels}
		for k, v := range a.Spec.Params {
			td.params[k] = v
		}
		for k, v := range t.Params {
			td.params[k] = v
		}
		td.info = map[string]any{"name": string(t.Name), "labels": orEmpty(t.Labels), "params": td.params}
		failed := false
		render := func(field, text string) string {
			out, err := config.RenderTemplate(field, text, td.params)
			if err != nil {
				c.errorf(a.Source, tokens, "provider %s, %s: %v", p.Metadata.Name, field, err)
				failed = true
			}
			return out
		}
		raw := render("baseURL", string(p.Spec.BaseURL))
		if !failed {
			u, err := url.Parse(raw)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				c.errorf(a.Source, tokens,
					"provider %s, baseURL: %q is not an absolute http or https URL (check the account params)", p.Metadata.Name, raw)
				failed = true
			} else {
				u.Path = strings.TrimSuffix(u.Path, "/")
				td.baseURL = u
			}
		}
		if d := p.Spec.Defaults; d != nil {
			for _, k := range sortedKeys(d.Headers) {
				td.headers[k] = render("defaults.headers."+k, string(d.Headers[k]))
			}
		}
		if c.opts.Secrets != nil {
			td.secrets = auth.Secrets{}
			refs := map[string]config.SecretRef{}
			for k, v := range a.Spec.Secrets {
				refs[k] = v
			}
			for k, v := range t.Secrets {
				refs[k] = v
			}
			for _, k := range sortedKeys(refs) {
				v, err := secret.Resolve(refs[k], *c.opts.Secrets)
				if err != nil {
					at := []string{"spec", "secrets", k}
					if _, own := t.Secrets[k]; own {
						at = []string{"spec", "targets", strconv.Itoa(i), "secrets", k}
					}
					c.errorf(a.Source, at, "%v", err)
					failed = true
					continue
				}
				td.secrets[k] = v
			}
			td.credential = credentialKey(c.opts.CredentialSalt, td.secrets)
		}
		if failed {
			data.ok = false
		}
		data.targets = append(data.targets, td)
	}
	return data
}

func orEmpty(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

// credentialKey is the keyed hash of a set of secrets: accounts sharing the
// same secret values share it, and it reveals nothing about them.
func credentialKey(salt []byte, secrets auth.Secrets) string {
	mac := hmac.New(sha256.New, salt)
	for _, k := range sortedKeys(secrets) {
		mac.Write([]byte(k))
		mac.Write([]byte{0})
		mac.Write([]byte(secrets[k].Reveal()))
		mac.Write([]byte{0})
	}
	return hex.EncodeToString(mac.Sum(nil))[:16]
}

// withImplicitParents returns the jobs plus an implicit job for every
// forEach.template that no job of the same account instantiates (see
// "Implementation notes" in 05-collection.md).
func (c *compiler) withImplicitParents(jobs []*config.Job) []*config.Job {
	out := slices.Clone(jobs)
	slices.SortStableFunc(out, func(a, b *config.Job) int { return strings.Compare(string(a.Metadata.Name), string(b.Metadata.Name)) })
	for _, j := range out {
		if t := j.Spec.Template; t != "" {
			key := [2]config.Name{j.Spec.Account, t}
			if _, ok := c.parents[key]; !ok {
				c.parents[key] = string(j.Metadata.Name)
			}
		}
	}
	var need [][2]config.Name
	requires := func(account config.Name, def *config.JobDefinition) {
		if fe := def.ForEach; fe != nil && fe.Template != "" {
			need = append(need, [2]config.Name{account, fe.Template})
		}
	}
	for _, j := range out {
		if t, ok := c.templates[j.Spec.Template]; ok {
			requires(j.Spec.Account, &t.Spec.JobDefinition)
		} else {
			requires(j.Spec.Account, &j.Spec.JobDefinition)
		}
	}
	for len(need) > 0 {
		key := need[0]
		need = need[1:]
		if _, ok := c.parents[key]; ok {
			continue
		}
		t, ok := c.templates[key[1]]
		if !ok {
			continue
		}
		name := string(key[0]) + "." + string(key[1])
		c.parents[key] = name
		c.implicit[name] = true
		out = append(out, &config.Job{
			Metadata: config.ObjectMeta{Name: config.Name(name)},
			Spec: config.JobSpec{
				Account: key[0], Template: key[1], Interval: t.Spec.DefaultInterval, Priority: config.PriorityLow,
			},
			Source: t.Source,
		})
		requires(key[0], &t.Spec.JobDefinition)
	}
	return out
}

func (c *compiler) compileJob(j *config.Job) {
	spec := &j.Spec
	acc := c.accounts[spec.Account]
	data := c.accData[spec.Account]
	p := c.providers[acc.Spec.Provider]
	if !data.ok {
		return
	}
	def, cd := &spec.JobDefinition, (*compiledDefinition)(nil)
	if spec.Template != "" {
		t := c.templates[spec.Template]
		def, cd = &t.Spec.JobDefinition, c.defs[spec.Template]
	} else {
		cd = c.compileDefinition(j.Source, def)
	}

	model := Job{
		Name:          string(j.Metadata.Name),
		Template:      string(j.Spec.Template),
		Account:       string(acc.Metadata.Name),
		Provider:      string(p.Metadata.Name),
		Implicit:      c.implicit[string(j.Metadata.Name)],
		Role:          def.Role,
		Interval:      spec.Interval.Std(),
		StaleAfter:    spec.StaleAfter.Std(),
		Priority:      spec.Priority,
		Extract:       cd.extract,
		Errors:        p.Spec.Errors,
		RedirectHosts: redirectHosts(p.Spec.Redirects),
		AccountInfo: map[string]any{
			"name": string(acc.Metadata.Name), "labels": orEmpty(acc.Spec.Labels), "params": orEmpty(acc.Spec.Params),
		},
		Limits: mapping.Limits{
			MaxItems:      c.graph.Settings.Mapping.MaxItemsPerRun,
			MaxSamples:    c.graph.Settings.Mapping.MaxSamplesPerRun,
			LabelConflict: c.graph.Settings.Mapping.LabelConflict,
		},
	}
	if model.Role == "" {
		model.Role = config.RoleMetrics
	}
	for _, o := range spec.Outputs {
		model.Outputs = append(model.Outputs, string(o))
	}
	if def.Estimate != nil {
		model.PagesEstimate = def.Estimate.PagesPerRun
	}
	if x := def.Extract; x != nil && x.Inventory != nil {
		model.InventoryTTL = x.Inventory.TTL.Std()
	}
	if fe := def.ForEach; fe != nil {
		model.ForEach = &ForEach{
			Parent: string(fe.Template) + string(fe.Job), Vars: sortedKeys(fe.Vars),
			programs: cd.forEach, filter: cd.filter, MaxInstances: fe.MaxInstances,
		}
		if fe.Job != "" {
			model.ForEach.ParentJob = string(fe.Job)
		} else {
			model.ForEach.ParentJob = c.parents[[2]config.Name{spec.Account, fe.Template}]
		}
	}
	switch {
	case cd.pagination != nil:
		model.Pagination = *cd.pagination
	case c.provPages[p.Metadata.Name] != nil:
		model.Pagination = *c.provPages[p.Metadata.Name]
	default:
		model.Pagination = Pagination{Type: config.PaginationNone, MaxPages: 1}
	}
	r := def.Request
	model.Request = Request{
		Method:     r.Method,
		Path:       r.Path,
		Timeout:    c.graph.Settings.Collection.DefaultTimeout.Std(),
		Idempotent: r.Idempotent,
	}
	if d := p.Spec.Defaults; d != nil && d.Timeout != 0 {
		model.Request.Timeout = d.Timeout.Std()
	}
	if r.Timeout != 0 {
		model.Request.Timeout = r.Timeout.Std()
	}

	for _, td := range data.targets {
		job := model
		job.Target = td.name
		job.Instance = instanceKey(job.Name, td.name, nil)
		job.BaseURL = td.baseURL
		job.Credential = td.credential
		job.Vars = maps.Clone(td.params)
		job.TargetInfo = td.info
		job.Labels = []map[string]string{acc.Spec.Labels, td.labels, spec.Labels}
		job.Request.Headers = map[string]string{}
		for k, v := range td.headers {
			job.Request.Headers[k] = v
		}
		for k, v := range r.Headers {
			job.Request.Headers[k] = v
		}
		job.Request.Query = c.params(j, "query", r.Query, cd.query, td)
		job.Request.Body = c.params(j, "body", r.Body, cd.body, td)
		if c.opts.Secrets != nil {
			a, err := c.opts.Auth.For(p.Spec.Auth, auth.Session{
				Scope:   auth.Scope{Provider: job.Provider, Account: job.Account, Target: job.Target, Credential: job.Credential},
				BaseURL: td.baseURL, Secrets: td.secrets, Vars: td.params, Account: job.AccountInfo, Target: td.info,
				Login: c.logins[p.Metadata.Name],
			})
			if err != nil {
				c.errorf(acc.Source, []string{"spec", "secrets"}, "provider %s: %v", p.Metadata.Name, err)
			}
			job.Auth = a
		}
		c.graph.Jobs = append(c.graph.Jobs, &job)
	}
}

func (c *compiler) params(j *config.Job, field string, values map[string]config.Value,
	exprs map[string]*mapping.Program, td *targetData,
) []Param {
	out := make([]Param, 0, len(values))
	for _, k := range sortedKeys(values) {
		v := values[k]
		p := Param{Name: k, Literal: v.Literal, Expr: exprs[k]}
		if v.Secret != "" && td.secrets != nil {
			s, ok := td.secrets[v.Secret]
			if !ok {
				c.errorf(j.Source, []string{"spec", "account"}, "request %s %s uses secret %q, which the account does not provide",
					field, k, v.Secret)
				continue
			}
			p.Secret = s
		}
		out = append(out, p)
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func redirectHosts(r *config.Redirects) []string {
	if r == nil {
		return nil
	}
	return r.Hosts
}
