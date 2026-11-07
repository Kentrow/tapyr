package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"text/tabwriter"

	"github.com/goccy/go-yaml"
	"github.com/spf13/cobra"

	"github.com/kentrow/tapyr/internal/auth"
	"github.com/kentrow/tapyr/internal/auth/plugins"
	"github.com/kentrow/tapyr/internal/budget"
	"github.com/kentrow/tapyr/internal/collect"
	"github.com/kentrow/tapyr/internal/collector"
	"github.com/kentrow/tapyr/internal/config"
	"github.com/kentrow/tapyr/internal/runtime"
	"github.com/kentrow/tapyr/internal/selfobs"
	"github.com/kentrow/tapyr/internal/sim"
)

// recordEntry is an instance of index.yaml.
type recordEntry struct {
	Job    string            `yaml:"job"`
	Method string            `yaml:"method"`
	Path   string            `yaml:"path"`
	Vars   map[string]string `yaml:"vars,omitempty"`
	Pages  []recordPage      `yaml:"pages,omitempty"`
	Error  string            `yaml:"error,omitempty"`
}

type recordPage struct {
	File    string            `yaml:"file"`
	Status  int               `yaml:"status"`
	Headers map[string]string `yaml:"headers,omitempty"`
}

func newRecordCmd(e *env, flags *globalFlags) *cobra.Command {
	var jobs []string
	var out, rules string
	cmd := &cobra.Command{
		Use:   "record --job J [--job ...] --out DIR [--anonymize FILE]",
		Short: "Record real API responses into anonymized fixtures",
		Long: "Run every instance of the selected jobs once, through the budget engine,\n" +
			"and write the responses, anonymized, into --out: one JSON file per page,\n" +
			"index.yaml and report.txt (the replaced paths, never the values). Parents\n" +
			"of the selected fan-out jobs run too. Review report.txt before committing\n" +
			"anything.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if len(jobs) == 0 || out == "" {
				return &exitError{code: exitFailure, err: errors.New("--job and --out are required")}
			}
			var extra sim.AnonymizeRules
			if rules != "" {
				data, err := os.ReadFile(rules)
				if err != nil {
					return &exitError{code: exitFailure, err: err}
				}
				if err := yaml.UnmarshalWithOptions(data, &extra, yaml.Strict(), yaml.UseJSONUnmarshaler()); err != nil {
					return &exitError{code: exitFailure, err: fmt.Errorf("%s: %w", rules, err)}
				}
			}
			anon, err := sim.NewAnonymizer(extra)
			if err != nil {
				return &exitError{code: exitFailure, err: fmt.Errorf("%s: %w", rules, err)}
			}
			cfg, err := loadConfig(e, flags)
			if err != nil {
				return err
			}
			redactor := selfobs.NewRedactor()
			secrets := &config.SecretSource{LookupEnv: e.lookupEnv, ReadFile: os.ReadFile, OnResolve: func(v string) { redactor.Add(v) }}
			logger, err := settingsLogger(e, flags, &cfg.Settings.Spec.Log, redactor)
			if err != nil {
				return &exitError{code: exitFailure, err: err}
			}
			tokens, err := auth.NewManager(auth.ManagerOptions{
				Clock: e.clock, Logger: logger, OnToken: func(token string) { redactor.Add(token) }, Plugins: plugins.Builtin(),
			})
			if err != nil {
				return &exitError{code: exitFailure, err: err}
			}
			graph, err := compileGraph(e, cfg, runtime.Options{Secrets: secrets, Auth: tokens})
			if err != nil {
				return err
			}
			selected, err := selectJobs(graph.Jobs, jobs)
			if err != nil {
				return &exitError{code: exitFailure, err: err}
			}
			engine, err := budget.NewEngine(budget.Config{
				Providers: cfg.Providers, Accounts: cfg.Accounts, RetryReserve: cfg.Settings.Spec.Budget.RetryReserve.Fraction(),
			}, e.clock, logger)
			if err != nil {
				return &exitError{code: exitFailure, err: err}
			}
			exec := collect.New(collect.Options{
				Budget: engine, Clock: e.clock, Logger: logger, Retry: cfg.Settings.Spec.Retry,
				MaxConcurrentRequests: cfg.Settings.Spec.Collection.MaxConcurrentRequests, ClientFor: tlsClients(graph, logger),
			})
			tokens.SetSender(exec)
			return record(cmd, e, recording{
				exec: exec, tokens: tokens, anon: anon, jobs: selected,
				headers: collector.KeptHeaders(cfg.Providers), out: out, redact: redactor.Redact,
			})
		},
	}
	cmd.Flags().StringArrayVar(&jobs, "job", nil, "job to record, repeatable")
	cmd.Flags().StringVar(&out, "out", "", "directory receiving the recording")
	cmd.Flags().StringVar(&rules, "anonymize", "", "file of additional anonymization rules (replace, keep)")
	addProviderPathFlag(cmd, flags)
	return cmd
}

type recording struct {
	exec    *collect.Executor
	tokens  *auth.Manager
	anon    *sim.Anonymizer
	jobs    []*runtime.Job
	headers map[string][]string // provider -> response headers kept
	out     string
	redact  func(string) string
}

func record(cmd *cobra.Command, e *env, r recording) error {
	ctx := cmd.Context()
	if err := os.MkdirAll(r.out, 0o750); err != nil {
		return &exitError{code: exitFailure, err: err}
	}
	var entries []recordEntry
	pages := map[string]int{}
	failed := 0
	var writeErr error
	var mu sync.Mutex
	r.exec.RunOnce(ctx, r.jobs, func(v collect.Visit) {
		mu.Lock()
		defer mu.Unlock()
		entry := recordEntry{Job: v.Job.Name, Method: v.Job.Request.Method, Path: v.Job.Request.Path}
		if entry.Method == "" {
			entry.Method = http.MethodGet
		}
		for _, name := range slices.Sorted(maps.Keys(v.Job.Vars)) {
			if entry.Vars == nil {
				entry.Vars = map[string]string{}
			}
			entry.Vars[name] = r.anon.Var(name, v.Job.Vars[name])
		}
		if v.Result == nil {
			failed++
			entry.Error = r.redact(v.Err.Error())
			entries = append(entries, entry)
			fmt.Fprintf(e.stderr, "%s: %s\n", v.Job.Name, entry.Error)
			return
		}
		for _, p := range v.Result.Pages {
			pages[v.Job.Name]++
			file := filepath.ToSlash(filepath.Join(v.Job.Name, fmt.Sprintf("%03d.json", pages[v.Job.Name])))
			data, err := json.MarshalIndent(r.anon.Document(p.Body), "", "  ")
			if err == nil {
				err = os.MkdirAll(filepath.Join(r.out, v.Job.Name), 0o750)
			}
			if err == nil {
				err = os.WriteFile(filepath.Join(r.out, filepath.FromSlash(file)), append(data, '\n'), 0o600)
			}
			if err != nil && writeErr == nil {
				writeErr = err
			}
			page := recordPage{File: file, Status: p.Status}
			for _, h := range r.headers[v.Job.Provider] {
				if val := p.Headers.Get(h); val != "" {
					if page.Headers == nil {
						page.Headers = map[string]string{}
					}
					page.Headers[h] = val
				}
			}
			entry.Pages = append(entry.Pages, page)
		}
		if v.Err != nil {
			failed++
			entry.Error = r.redact(v.Err.Error())
			fmt.Fprintf(e.stderr, "%s: %s\n", v.Job.Name, entry.Error)
		}
		entries = append(entries, entry)
	})
	r.tokens.Close(ctx)
	if writeErr != nil {
		return &exitError{code: exitFailure, err: writeErr}
	}
	index, err := yaml.Marshal(entries)
	if err != nil {
		return &exitError{code: exitFailure, err: err}
	}
	header := "# Recorded by tapyr record, anonymized: review report.txt before committing anything.\n"
	if err := os.WriteFile(filepath.Join(r.out, "index.yaml"), append([]byte(header), index...), 0o600); err != nil {
		return &exitError{code: exitFailure, err: err}
	}
	var report bytes.Buffer
	writeReport(&report, r.anon.Report())
	if err := os.WriteFile(filepath.Join(r.out, "report.txt"), report.Bytes(), 0o600); err != nil {
		return &exitError{code: exitFailure, err: err}
	}
	total := 0
	for _, n := range pages {
		total += n
	}
	fmt.Fprintf(e.stdout, "recorded %d pages of %d instances into %s; review %s\n", total, len(entries), r.out,
		filepath.Join(r.out, "report.txt"))
	if failed > 0 {
		return &exitError{code: exitFailure, err: fmt.Errorf("%d of %d instances failed", failed, len(entries))}
	}
	return nil
}

func writeReport(w io.Writer, report []sim.Replacement) {
	fmt.Fprintln(w, "Values replaced by tapyr record (paths and rules only). Check that nothing")
	fmt.Fprintln(w, "identifying is left in the other fields before committing the recording.")
	fmt.Fprintln(w)
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "PATH\tRULE\tCOUNT")
	for _, r := range report {
		fmt.Fprintf(tw, "%s\t%s\t%d\n", r.Path, r.Rule, r.Count)
	}
	_ = tw.Flush()
}

// selectJobs returns the instances of the named jobs and of their parents,
// up to the root of their fan-out.
func selectJobs(jobs []*runtime.Job, names []string) ([]*runtime.Job, error) {
	byName := map[string][]*runtime.Job{}
	for _, j := range jobs {
		byName[j.Name] = append(byName[j.Name], j)
	}
	want := map[string]bool{}
	var add func(name string) error
	add = func(name string) error {
		js, ok := byName[name]
		if !ok {
			return fmt.Errorf("no job %q in the configuration", name)
		}
		if want[name] {
			return nil
		}
		want[name] = true
		for _, j := range js {
			if j.ForEach != nil {
				return add(j.ForEach.ParentJob)
			}
		}
		return nil
	}
	for _, n := range names {
		if err := add(n); err != nil {
			return nil, err
		}
	}
	var out []*runtime.Job
	for _, j := range jobs {
		if want[j.Name] {
			out = append(out, j)
		}
	}
	return out, nil
}
