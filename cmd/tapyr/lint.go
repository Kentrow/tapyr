package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/kentrow/tapyr/internal/auth"
	"github.com/kentrow/tapyr/internal/auth/plugins"
	"github.com/kentrow/tapyr/internal/budget"
	"github.com/kentrow/tapyr/internal/clock"
	"github.com/kentrow/tapyr/internal/collect"
	"github.com/kentrow/tapyr/internal/collector"
	"github.com/kentrow/tapyr/internal/config"
	"github.com/kentrow/tapyr/internal/model"
	"github.com/kentrow/tapyr/internal/provider"
	"github.com/kentrow/tapyr/internal/runtime"
	"github.com/kentrow/tapyr/internal/series"
	"github.com/kentrow/tapyr/internal/sim"
)

// Contract test files of a provider package (see docs/spec/08-simulator.md).
const (
	contractFile  = "tests/contract.yaml"
	contractEnv   = "tests/contract.env"
	expectedFile  = "tests/expected.prom"
	simURLMarker  = "$SIM_URL"  // the simulator URL with its base path
	simAddrMarker = "$SIM_ADDR" // the simulator URL without its base path
	contractSeed  = 42
	contractLimit = 2 * time.Minute // real time, for a contract run
)

// contractStart is the simulated time of the contract tests.
var contractStart = time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)

func newProviderLintCmd(e *env) *cobra.Command {
	var update, catalog bool
	cmd := &cobra.Command{
		Use:   "lint <dir>",
		Short: "Validate a provider package and run its contract tests",
		Long: "Validate a provider package (or every package of a directory): schema, CEL\n" +
			"compilation, dashboards, and the contract test of tests/contract.yaml, run\n" +
			"once against the package simulator at a fixed simulated time. The samples\n" +
			"must equal tests/expected.prom; --update rewrites it instead. --catalog also\n" +
			"requires the catalog metadata: display name, categories, maintainers, tier,\n" +
			"validation and the source of every limit.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			loader, builtin, err := newLoader()
			if err != nil {
				return err
			}
			pkgs, err := provider.FromPath(loader, args[0])
			if err != nil {
				return &exitError{code: exitInvalidConfig, err: err}
			}
			if len(pkgs) == 0 {
				return &exitError{code: exitInvalidConfig, err: fmt.Errorf("%s holds no provider package", args[0])}
			}
			var cats []category
			if catalog {
				if cats, err = catalogCategories(); err != nil {
					return err
				}
			}
			failed := 0
			for _, pkg := range pkgs {
				dir := args[0]
				if len(pkgs) > 1 || filepath.Base(filepath.Clean(dir)) != string(pkg.Provider.Metadata.Name) {
					dir = filepath.Join(args[0], string(pkg.Provider.Metadata.Name))
				}
				problems := lintPackage(cmd.Context(), loader, pkg, builtin, dir, update)
				if catalog {
					problems = append(problems, lintCatalog(pkg, cats)...)
				}
				name := pkg.Provider.Metadata.Name
				if len(problems) == 0 {
					fmt.Fprintf(e.stdout, "%s: ok\n", name)
					continue
				}
				failed++
				for _, p := range problems {
					fmt.Fprintf(e.stderr, "%s: %s\n", name, p)
				}
			}
			if failed > 0 {
				return &exitError{code: exitInvalidConfig, err: fmt.Errorf("%d of %d packages failed the lint", failed, len(pkgs))}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&update, "update", false, "write tests/expected.prom instead of comparing")
	cmd.Flags().BoolVar(&catalog, "catalog", false, "also require the catalog metadata (ADR 0011)")
	return cmd
}

// lintPackage returns the problems of a package; dir is its directory, for
// --update.
func lintPackage(ctx context.Context, loader *config.Loader, pkg *config.Package, builtin []*config.Package, dir string, update bool) []string {
	others := slices.DeleteFunc(slices.Clone(builtin), func(p *config.Package) bool {
		return p.Provider.Metadata.Name == pkg.Provider.Metadata.Name
	})
	pkgs := append([]*config.Package{pkg}, others...)
	var problems []string
	// The templates compile on their own (CEL, Go templates), used or not.
	cfg, err := config.Assemble(&config.Config{}, pkgs)
	var tokens *auth.Manager
	if err == nil {
		tokens, err = offlineAuth(clock.NewFake(contractStart))
	}
	if err == nil {
		_, err = runtime.Compile(cfg, runtime.Options{Auth: tokens})
	}
	if err != nil {
		return append(problems, diagLines(err)...)
	}
	metrics := packageMetrics(pkg)
	problems = append(problems, lintDashboards(pkg, metrics)...)
	problems = append(problems, lintExamples(loader, pkg, pkgs, dir)...)
	problems = append(problems, lintScenarios(pkg)...)
	if _, err := fs.Stat(pkg.FS, contractFile); errors.Is(err, fs.ErrNotExist) {
		return append(problems, "no contract test ("+contractFile+")")
	}
	samples, used, runProblems := runContract(ctx, loader, pkg, pkgs)
	problems = append(problems, runProblems...)
	for _, t := range pkg.JobTemplates {
		if !used[string(t.Metadata.Name)] {
			problems = append(problems, fmt.Sprintf("template %s is not exercised by %s", t.Metadata.Name, contractFile))
		}
	}
	if len(runProblems) > 0 {
		return problems
	}
	got := formatSamples(samples)
	if update {
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(expectedFile)), got, 0o600); err != nil {
			problems = append(problems, fmt.Sprintf("writing %s: %v", expectedFile, err))
		}
		return problems
	}
	want, err := fs.ReadFile(pkg.FS, expectedFile)
	if err != nil {
		return append(problems, fmt.Sprintf("reading %s: %v (run with --update to create it)", expectedFile, err))
	}
	if diff := diffLines(string(want), string(got)); diff != "" {
		problems = append(problems, fmt.Sprintf("the samples differ from %s (run with --update to accept them):\n%s", expectedFile, diff))
	}
	return problems
}

func diagLines(err error) []string {
	var diags config.Diagnostics
	if errors.As(err, &diags) {
		out := make([]string, 0, len(diags))
		for _, d := range diags {
			out = append(out, d.String())
		}
		return out
	}
	return []string{err.Error()}
}

// runContract runs the jobs of the contract test once against the
// simulator, on a fake clock that moves only when the run waits (budget,
// backoff, simulated latency). It returns the samples, the templates used
// and the problems met.
func runContract(ctx context.Context, loader *config.Loader, pkg *config.Package, pkgs []*config.Package) ([]model.Sample, map[string]bool, []string) {
	ctx, cancel := context.WithTimeout(ctx, contractLimit)
	defer cancel()
	logger := slog.New(slog.DiscardHandler)
	clk := clock.NewFake(contractStart)
	srv, err := sim.New(pkg, sim.Options{Clock: clk, Seed: contractSeed, Logger: logger, Plugins: plugins.Builtin()})
	if err != nil {
		return nil, nil, []string{err.Error()}
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return nil, nil, []string{err.Error()}
	}
	server := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = server.Serve(ln) }()
	defer server.Close()

	tmp, err := os.MkdirTemp("", "tapyr-contract-")
	if err != nil {
		return nil, nil, []string{err.Error()}
	}
	defer os.RemoveAll(tmp)
	doc, err := fs.ReadFile(pkg.FS, contractFile)
	if err != nil {
		return nil, nil, []string{err.Error()}
	}
	simAddr := "http://" + ln.Addr().String()
	simURL := simAddr + strings.TrimSuffix(pkg.Simulation.Spec.BasePath, "/")
	doc = bytes.ReplaceAll(bytes.ReplaceAll(doc, []byte(simURLMarker), []byte(simURL)), []byte(simAddrMarker), []byte(simAddr))
	path := filepath.Join(tmp, "contract.yaml")
	if err := os.WriteFile(path, doc, 0o600); err != nil {
		return nil, nil, []string{err.Error()}
	}
	vars, err := contractVars(pkg)
	if err != nil {
		return nil, nil, []string{err.Error()}
	}
	cfg, err := loader.Parse([]string{path})
	if err == nil {
		cfg, err = config.Assemble(cfg, pkgs)
	}
	if err != nil {
		return nil, nil, rewrite(diagLines(err), path, contractFile)
	}
	tokens, err := auth.NewManager(auth.ManagerOptions{Clock: clk, Logger: logger, Plugins: plugins.Builtin()})
	if err != nil {
		return nil, nil, []string{err.Error()}
	}
	graph, err := runtime.Compile(cfg, runtime.Options{
		Auth: tokens,
		Secrets: &config.SecretSource{
			LookupEnv: func(k string) (string, bool) { v, ok := vars[k]; return v, ok },
			ReadFile: func(p string) ([]byte, error) {
				return nil, fmt.Errorf("%s: contract tests read secrets from %s only", p, contractEnv)
			},
		},
	})
	if err != nil {
		return nil, nil, rewrite(diagLines(err), path, contractFile)
	}
	engine, err := budget.NewEngine(budget.Config{Providers: cfg.Providers, Accounts: cfg.Accounts}, clk, logger)
	if err != nil {
		return nil, nil, []string{err.Error()}
	}
	exec := collect.New(collect.Options{
		Budget: engine, Clock: clk, Logger: logger, Retry: cfg.Settings.Spec.Retry,
		ClientFor: tlsClients(graph, nil),
	})
	tokens.SetSender(exec)

	var samples []model.Sample
	var problems []string
	used := map[string]bool{}
	accum := series.NewAccumulators()
	done := make(chan struct{})
	go func() {
		defer close(done)
		exec.RunOnce(ctx, graph.Jobs, func(v collect.Visit) {
			used[v.Job.Template] = true
			if v.Err != nil {
				problems = append(problems, fmt.Sprintf("%s: %v", v.Job.Instance, v.Err))
				return
			}
			for _, err := range v.Output.Errors {
				problems = append(problems, fmt.Sprintf("%s: mapping: %v", v.Job.Instance, err))
			}
			for _, err := range v.Skipped {
				problems = append(problems, fmt.Sprintf("%s: inventory item skipped: %v", v.Job.Instance, err))
			}
			samples = append(samples, accum.Apply(v.Job.Instance, v.Output.Samples)...)
		})
		tokens.Close(ctx)
	}()
	driveClock(ctx, clk, done)
	if ctx.Err() != nil {
		problems = append(problems, "the contract run did not finish in "+contractLimit.String())
	}
	return samples, used, problems
}

// driveClock moves the fake clock to the next deadline whenever the run
// waits on it, until done.
func driveClock(ctx context.Context, clk *clock.Fake, done <-chan struct{}) {
	tick := time.NewTicker(time.Millisecond) //nolint:forbidigo // polls, in real time, the fake clock it drives.
	defer tick.Stop()
	for {
		select {
		case <-done:
			return
		case <-ctx.Done():
			<-done
			return
		case <-tick.C:
			if next, ok := clk.Next(); ok {
				clk.Set(next)
			}
		}
	}
}

// tlsClients returns the HTTP clients of the accounts with TLS settings,
// and logs, when logger is set, the accounts that disable certificate
// verification.
func tlsClients(graph *runtime.Graph, logger *slog.Logger) func(string) *http.Client {
	clients := map[string]*http.Client{}
	for _, account := range slices.Sorted(maps.Keys(graph.TLS)) {
		t := graph.TLS[account]
		if t.Insecure && logger != nil {
			logger.Warn("TLS certificate verification is disabled (insecureSkipVerify)", "account", account)
		}
		clients[account] = collector.TLSClient(t)
	}
	return func(account string) *http.Client { return clients[account] }
}

// contractVars reads tests/contract.env: KEY=VALUE lines, comments with #.
// Contract tests never read the real environment.
func contractVars(pkg *config.Package) (map[string]string, error) {
	vars := map[string]string{}
	data, err := fs.ReadFile(pkg.FS, contractEnv)
	if errors.Is(err, fs.ErrNotExist) {
		return vars, nil
	}
	if err != nil {
		return nil, err
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("%s:%d: expected KEY=VALUE", contractEnv, n)
		}
		vars[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return vars, nil
}

// rewrite replaces the temporary path of the contract file in messages.
func rewrite(lines []string, from, to string) []string {
	for i, l := range lines {
		lines[i] = strings.ReplaceAll(l, from, to)
	}
	return lines
}

// formatSamples writes samples as a sorted Prometheus exposition without
// timestamps.
func formatSamples(samples []model.Sample) []byte {
	type line struct{ name, labels, value string }
	types := map[string]string{}
	lines := make([]line, 0, len(samples))
	for _, s := range samples {
		types[s.Name] = s.Type.String()
		var b strings.Builder
		for i, l := range s.Labels {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(l.Name + "=" + strconv.Quote(l.Value))
		}
		lines = append(lines, line{s.Name, b.String(), strconv.FormatFloat(s.Value, 'g', -1, 64)})
	}
	slices.SortFunc(lines, func(a, b line) int {
		if c := strings.Compare(a.name, b.name); c != 0 {
			return c
		}
		return strings.Compare(a.labels, b.labels)
	})
	var out bytes.Buffer
	last := ""
	for _, l := range lines {
		if l.name != last {
			fmt.Fprintf(&out, "# TYPE %s %s\n", l.name, types[l.name])
			last = l.name
		}
		if l.labels == "" {
			fmt.Fprintf(&out, "%s %s\n", l.name, l.value)
		} else {
			fmt.Fprintf(&out, "%s{%s} %s\n", l.name, l.labels, l.value)
		}
	}
	return out.Bytes()
}

// diffLines lists the lines missing from got (-) and the unexpected ones
// (+), at most 40.
func diffLines(want, got string) string {
	w, g := strings.Split(want, "\n"), strings.Split(got, "\n")
	inW, inG := map[string]bool{}, map[string]bool{}
	for _, l := range w {
		inW[l] = true
	}
	for _, l := range g {
		inG[l] = true
	}
	var out []string
	for _, l := range w {
		if !inG[l] {
			out = append(out, "- "+l)
		}
	}
	for _, l := range g {
		if !inW[l] {
			out = append(out, "+ "+l)
		}
	}
	if len(out) > 40 {
		out = append(out[:40], fmt.Sprintf("... %d more", len(out)-40))
	}
	return strings.Join(out, "\n")
}

// lintExamples checks that the example documents of a package load with
// it (secrets are not resolved).
func lintExamples(loader *config.Loader, pkg *config.Package, pkgs []*config.Package, dir string) []string {
	files, err := fs.Glob(pkg.FS, "examples/*.yaml")
	if err != nil || len(files) == 0 {
		return []string{"no example (examples/*.yaml)"}
	}
	if _, err := os.Stat(filepath.Join(dir, "examples")); err != nil {
		return nil // an embedded package: its examples are linted from the source tree
	}
	cfg, err := loader.Parse([]string{filepath.Join(dir, "examples")})
	if err == nil {
		cfg, err = config.Assemble(cfg, pkgs)
	}
	var tokens *auth.Manager
	if err == nil {
		tokens, err = offlineAuth(clock.NewFake(contractStart))
	}
	if err == nil {
		_, err = runtime.Compile(cfg, runtime.Options{Auth: tokens})
	}
	if err != nil {
		return diagLines(err)
	}
	return nil
}

// requiredScenarios are the scenarios every provider simulator ships
// (docs/spec/10-providers.md, checklist).
var requiredScenarios = []string{"default", "quota-tight", "bad-day"}

// lintScenarios checks that the simulator ships the required scenarios and
// that every scenario loads with it.
func lintScenarios(pkg *config.Package) []string {
	if pkg.Simulation == nil {
		return []string{"no simulator (" + config.PackageSimulationFile + ")"}
	}
	var problems []string
	for _, name := range requiredScenarios {
		if pkg.Scenarios[name] == nil {
			problems = append(problems, fmt.Sprintf("no %s scenario (sim/scenarios/%s.yaml)", name, name))
		}
	}
	for _, name := range slices.Sorted(maps.Keys(pkg.Scenarios)) {
		if _, err := sim.New(pkg, sim.Options{Scenario: pkg.Scenarios[name], Clock: clock.NewFake(contractStart), Plugins: plugins.Builtin()}); err != nil {
			problems = append(problems, fmt.Sprintf("scenario %s: %v", name, err))
		}
	}
	return problems
}

// packageMetrics returns the metric names produced by the templates of a
// package.
func packageMetrics(pkg *config.Package) map[string]bool {
	out := map[string]bool{}
	for _, t := range pkg.JobTemplates {
		for _, name := range templateMetrics(t) {
			out[name] = true
		}
	}
	return out
}

// identifierPattern matches PromQL identifiers.
var identifierPattern = regexp.MustCompile(`[a-zA-Z_:][a-zA-Z0-9_:]*`)

// lintDashboards checks the Grafana dashboards of a package: valid JSON, a
// datasource variable, queries on metrics of the package or tapyr_
// self-metrics only, and the conventions of panelProblems.
func lintDashboards(pkg *config.Package, metrics map[string]bool) []string {
	files, err := fs.Glob(pkg.FS, "dashboards/*.json")
	if err != nil || len(files) == 0 {
		return []string{"no dashboard (dashboards/*.json)"}
	}
	prefixes := map[string]bool{}
	for m := range metrics {
		if p, _, ok := strings.Cut(m, "_"); ok {
			prefixes[p+"_"] = true
		}
	}
	var problems []string
	for _, f := range files {
		data, err := fs.ReadFile(pkg.FS, f)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", f, err))
			continue
		}
		var d struct {
			Title      string `json:"title"`
			UID        string `json:"uid"`
			Templating struct {
				List []struct {
					Type string `json:"type"`
					Name string `json:"name"`
				} `json:"list"`
			} `json:"templating"`
			Panels []dashboardPanel `json:"panels"`
		}
		if err := json.Unmarshal(data, &d); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", f, err))
			continue
		}
		if d.Title == "" || d.UID == "" {
			problems = append(problems, f+": the dashboard needs a title and a uid")
		}
		if !slices.ContainsFunc(d.Templating.List, func(v struct {
			Type string `json:"type"`
			Name string `json:"name"`
		},
		) bool {
			return v.Type == "datasource"
		}) {
			problems = append(problems, f+": no datasource variable")
		}
		unknown := map[string]bool{}
		var walk func([]dashboardPanel)
		walk = func(panels []dashboardPanel) {
			for _, p := range panels {
				for _, t := range p.Targets {
					for _, id := range identifierPattern.FindAllString(stripStrings(t.Expr), -1) {
						if known(id, metrics, prefixes) {
							continue
						}
						unknown[id] = true
					}
				}
				walk(p.Panels)
			}
		}
		walk(d.Panels)
		problems = append(problems, panelProblems(f, d.Panels)...)
		for _, id := range slices.Sorted(maps.Keys(unknown)) {
			problems = append(problems, fmt.Sprintf("%s: %s is not a metric of the package", f, id))
		}
	}
	return problems
}

type dashboardPanel struct {
	Type        string `json:"type"`
	Title       string `json:"title"`
	FieldConfig struct {
		Defaults struct {
			Thresholds json.RawMessage `json:"thresholds"`
		} `json:"defaults"`
	} `json:"fieldConfig"`
	Targets []struct {
		Expr string `json:"expr"`
	} `json:"targets"`
	Panels []dashboardPanel `json:"panels"`
}

// panelProblems checks the dashboard conventions a reader can miss
// (docs/providers.md, "Dashboard conventions"): a stat, gauge or bar gauge
// declares its thresholds, else Grafana colors it red from 80.
func panelProblems(file string, panels []dashboardPanel) []string {
	var problems []string
	for _, p := range panels {
		switch p.Type {
		case "stat", "gauge", "bargauge":
			if len(p.FieldConfig.Defaults.Thresholds) == 0 {
				problems = append(problems, fmt.Sprintf("%s: panel %q has no thresholds (Grafana colors it red from 80)", file, p.Title))
			}
		}
		problems = append(problems, panelProblems(file, p.Panels)...)
	}
	return problems
}

// stripStrings removes quoted strings (label values, regexps) and
// $variables from a PromQL expression.
func stripStrings(expr string) string {
	var b strings.Builder
	quote := rune(0)
	for i, r := range expr {
		switch {
		case quote != 0:
			if r == quote && (i == 0 || expr[i-1] != '\\') {
				quote = 0
			}
		case r == '"' || r == '\'' || r == '`':
			quote = r
		default:
			b.WriteRune(r)
		}
	}
	return regexp.MustCompile(`\$\{?[a-zA-Z_][a-zA-Z0-9_]*\}?`).ReplaceAllString(b.String(), " ")
}

// known reports whether an identifier of a query is fine: a metric of the
// package (with the suffixes of the metric types), or an identifier that
// does not look like one (functions, keywords, labels).
func known(id string, metrics, prefixes map[string]bool) bool {
	if metrics[id] || strings.HasPrefix(id, "tapyr_") {
		return true
	}
	p, _, ok := strings.Cut(id, "_")
	if !ok || !prefixes[p+"_"] {
		return true
	}
	// A label named like a metric prefix (rare) would land here: package
	// metric prefixes are not used as label names.
	return false
}
