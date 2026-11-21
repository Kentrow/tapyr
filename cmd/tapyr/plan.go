package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/kentrow/tapyr/internal/budget"
	"github.com/kentrow/tapyr/internal/budget/planner"
	"github.com/kentrow/tapyr/internal/buildinfo"
	"github.com/kentrow/tapyr/internal/collector"
	"github.com/kentrow/tapyr/internal/config"
	"github.com/kentrow/tapyr/internal/problem"
	"github.com/kentrow/tapyr/internal/processing"
	"github.com/kentrow/tapyr/internal/runtime"
	"github.com/kentrow/tapyr/internal/series"
	"github.com/kentrow/tapyr/internal/state"
)

func newPlanCmd(e *env, flags *globalFlags) *cobra.Command {
	var fanouts []string
	var format string
	cmd := &cobra.Command{
		Use:   "plan",
		Short: "Estimate quota consumption and effective intervals",
		Long: "Compute, offline, the effective interval of every job and the projected\n" +
			"consumption of every quota scope over a full window. Estimates use the run\n" +
			"history of the state store when it exists, else estimate.pagesPerRun.\n" +
			"Exit code 3 when the plan is infeasible.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if format != "table" && format != "json" {
				return &exitError{code: exitFailure, err: fmt.Errorf("invalid --output %q (want table or json)", format)}
			}
			hints, err := parseFanouts(fanouts)
			if err != nil {
				return &exitError{code: exitFailure, err: err}
			}
			cfg, err := loadConfig(e, flags)
			if err != nil {
				return err
			}
			tokens, err := offlineAuth(e.clock)
			if err != nil {
				return &exitError{code: exitFailure, err: err}
			}
			graph, err := compileGraph(e, cfg, runtime.Options{Auth: tokens})
			if err != nil {
				return err
			}
			warnNotCollected(e, graph)
			warnNoOutput(e, cfg)
			engine, err := newEngine(e, cfg, graph)
			if err != nil {
				return &exitError{code: exitFailure, err: err}
			}
			ctx := cmd.Context()
			var store state.Store
			if s, err := state.OpenSQLite(ctx, cfg.Settings.Spec.State.Path, true); err == nil {
				defer s.Close()
				store = s
				if entries, err := s.LoadLedger(ctx); err == nil {
					engine.Restore(entries)
				}
			} else if !errors.Is(err, fs.ErrNotExist) {
				return &exitError{code: exitFailure, err: err}
			}
			var entries []collector.PlanEntry
			for _, j := range graph.Jobs {
				fanout := 1
				if j.ForEach != nil {
					if n, ok := hints[j.ForEach.Parent]; ok {
						fanout = n
					}
				}
				entries = append(entries, collector.PlanEntry{Job: j, Fanout: fanout})
			}
			jobs, err := collector.PlanInputs(ctx, entries, engine, store)
			if err != nil {
				return &exitError{code: exitFailure, err: err}
			}
			for name := range hints {
				fanOut := func(j *runtime.Job) bool { return j.ForEach != nil && j.ForEach.Parent == name }
				if !slices.ContainsFunc(graph.Jobs, fanOut) && !slices.ContainsFunc(graph.NotCollected, fanOut) {
					fmt.Fprintf(e.stderr, "warning: --fanout %s matches no fan-out job\n", name)
				}
			}
			res := planner.Plan(jobs, planner.Options{MaxStretch: graph.Settings.Budget.MaxStretch, Now: e.clock.Now()})
			notCollected := graph.NotCollectedJobs()
			outputs, err := outputEstimates(ctx, graph, store, res)
			if err != nil {
				return &exitError{code: exitFailure, err: err}
			}
			if format == "json" {
				err = printPlanJSON(e, res, notCollected, outputs)
			} else {
				err = printPlanTable(e, res, notCollected, outputs, implicitJobs(graph.Jobs))
			}
			if err != nil {
				return err
			}
			if !res.Feasible {
				return &exitError{code: exitInfeasible, silent: true, err: errors.New("plan infeasible")}
			}
			return nil
		},
	}
	cmd.Flags().StringArrayVar(&fanouts, "fanout", nil, "fan-out hint: <parent>=<instances>, the parent as named in forEach (its template, or its job with forEach.job), repeatable")
	cmd.Flags().StringVar(&format, "output", "table", "output format: table or json")
	addProviderPathFlag(cmd, flags)
	return cmd
}

func parseFanouts(values []string) (map[string]int, error) {
	out := map[string]int{}
	for _, v := range values {
		name, n, ok := strings.Cut(v, "=")
		count, err := strconv.Atoi(n)
		if !ok || name == "" || err != nil || count < 1 {
			return nil, fmt.Errorf("invalid --fanout %q (want <parent>=<instances>)", v)
		}
		out[name] = count
	}
	return out, nil
}

// newEngine builds the budget engine of a configuration.
func newEngine(e *env, cfg *config.Config, graph *runtime.Graph) (*budget.Engine, error) {
	return budget.NewEngine(budget.Config{
		Providers: cfg.Providers, Accounts: cfg.Accounts,
		RetryReserve: graph.Settings.Budget.RetryReserve.Fraction(),
		Breaker:      graph.Settings.CircuitBreaker,
	}, e.clock, e.logger)
}

func printPlanTable(e *env, res *planner.Result, notCollected []*runtime.Job, outputs []outputEstimate, implicit []*runtime.Job) error {
	w := tabwriter.NewWriter(e.stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "QUOTA\tWINDOW\tAVAILABLE\tPROJECTED\tFOR RETRIES\tSTATUS")
	planned := map[string]bool{}
	for _, s := range res.Scopes {
		status := "fits"
		if !s.Feasible {
			status = "DOES NOT FIT"
		}
		if s.Budget > 0 {
			status += fmt.Sprintf(" (%.1f%%)", 100*s.Projected/s.Budget)
		}
		fmt.Fprintf(w, "%s\t%s\t%.0f\t%.0f\t%.0f\t%s\n", s.Limit, shortDuration(s.Period), s.Budget, s.Projected, s.Reserve, status)
		if words := strings.TrimPrefix(planner.ScopeWords(s.Key), " of "); words != "" {
			fmt.Fprintf(w, "  %s\t\t\t\t\t\n", words)
		}
		for _, j := range res.Jobs {
			calls, ok := j.Calls[s.ID]
			if !ok {
				continue
			}
			planned[j.Name] = true
			runs := s.Period.Seconds() / j.Effective.Seconds()
			line := fmt.Sprintf("  job %s\tdesired %s\teffective %s\t%.0f runs x %s = %.0f",
				j.Name, shortDuration(j.Desired), shortDuration(j.Effective), runs, pagesLabel(j), calls)
			if j.Effective != j.Desired {
				line += fmt.Sprintf("\t(slowed down x%s, priority %s, %s)", strconv.FormatFloat(math.Round(j.Stretch*100)/100, 'f', -1, 64), j.Priority, j.Describe())
			}
			fmt.Fprintln(w, line+"\t")
		}
	}
	var others []planner.JobPlan
	for _, j := range res.Jobs {
		if !planned[j.Name] {
			others = append(others, j)
		}
	}
	if len(others) > 0 {
		fmt.Fprintln(w, "\nJOBS WITHOUT QUOTA\t\t\t\t\t")
		for _, j := range others {
			line := fmt.Sprintf("  job %s\tdesired %s\teffective %s", j.Name, shortDuration(j.Desired), shortDuration(j.Effective))
			if j.Effective != j.Desired {
				line += "\t(" + j.Describe() + ")"
			}
			fmt.Fprintln(w, line+"\t")
		}
	}
	// Each section has its own columns: a long line of one never widens
	// the columns of another.
	if err := w.Flush(); err != nil {
		return err
	}
	if len(implicit) > 0 {
		iw := tabwriter.NewWriter(e.stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(iw, "\nImplicit jobs, run at priority low to list the items of a fan-out that no job declares:")
		for _, j := range implicit {
			fmt.Fprintf(iw, "  %s\tlists the %s for %s\n", j.Name, j.Items, strings.Join(j.DiscoveryFor, ", "))
		}
		if err := iw.Flush(); err != nil {
			return err
		}
	}
	if len(outputs) > 0 {
		if err := printOutputEstimates(e.stdout, outputs); err != nil {
			return err
		}
	}
	if len(notCollected) > 0 {
		nw := tabwriter.NewWriter(e.stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(nw, "\nJOBS NOT COLLECTED\t\t\t")
		for _, j := range notCollected {
			fmt.Fprintf(nw, "  job %s\tdesired %s\t%s\t\n", j.Name, shortDuration(j.Interval), j.NotCollected)
		}
		if err := nw.Flush(); err != nil {
			return err
		}
	}
	_, err := fmt.Fprint(e.stdout, planConclusion(res))
	return err
}

// implicitJobs returns the collected implicit jobs, one per name.
func implicitJobs(jobs []*runtime.Job) []*runtime.Job {
	var out []*runtime.Job
	for _, j := range jobs {
		if j.Implicit && !slices.ContainsFunc(out, func(o *runtime.Job) bool { return o.Name == j.Name }) {
			out = append(out, j)
		}
	}
	slices.SortFunc(out, func(a, b *runtime.Job) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// planConclusion says what the plan means, in words: it fits as desired,
// it fits by slowing jobs down, or it does not fit, with why and what to
// change for every quota it does not fit.
func planConclusion(res *planner.Result) string {
	if !res.Feasible {
		var b strings.Builder
		b.WriteString("\nThe plan does not fit:\n")
		for _, s := range res.Scopes {
			if p := problem.ForPlan(s, buildinfo.Get().Version); p != nil {
				b.WriteString(wrapWords("  ", strings.Fields(p.Title), 100))
				b.WriteString(wrapWords("  ", strings.Fields(p.Action), 100))
				b.WriteString("  How to fix: " + p.DocsURL + "\n")
			}
		}
		return b.String()
	}
	slowed := 0
	for _, j := range res.Jobs {
		if j.Effective != j.Desired {
			slowed++
		}
	}
	if slowed == 0 {
		return "\nThe plan fits: every job runs at its desired interval.\n"
	}
	return fmt.Sprintf("\nThe plan fits by slowing down %d of %d jobs to stay within the quotas (see effective above).\n", slowed, len(res.Jobs))
}

// printOutputEstimates prints what every output receives (ADR 0014). An
// output none of whose jobs has run yet says so instead of a count of 0;
// the jobs not measured yet are listed below the table, wrapped.
func printOutputEstimates(out io.Writer, outputs []outputEstimate) error {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "\nOUTPUT\tSERIES\tSAMPLES/MIN\tLIMIT\t")
	for _, o := range outputs {
		series, samples, limit, note := strconv.Itoa(o.Series), fmt.Sprintf("%.0f", o.SamplesPerMinute), "-", ""
		switch {
		case o.measured == 0 && len(o.Unmeasured) > 0:
			series, samples, note = "unknown", "-", "until its jobs have run once"
		case len(o.Unmeasured) > 0:
			series, samples = "at least "+series, "at least "+samples
		}
		if o.MaxSeries > 0 {
			limit = strconv.Itoa(o.MaxSeries)
			if o.Series > o.MaxSeries {
				note = fmt.Sprintf("about %d new series would be held back", o.Series-o.MaxSeries)
			}
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", o.Name, series, samples, limit, note)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	printBufferEstimates(out, outputs)
	header := false
	for _, o := range outputs {
		if len(o.Unmeasured) == 0 {
			continue
		}
		if !header {
			fmt.Fprintln(out, "\nNot measured yet (no successful run, their series are not counted):")
			header = true
		}
		fmt.Fprint(out, wrapList("  "+o.Name+": ", o.Unmeasured, 80))
	}
	return nil
}

// printBufferEstimates says how long an outage of the backend the disk
// buffer of every output holds (ADR 0015).
func printBufferEstimates(out io.Writer, outputs []outputEstimate) {
	header := false
	for _, o := range outputs {
		if o.BufferMaxBytes == 0 {
			continue
		}
		if !header {
			fmt.Fprintf(out, "\nDisk buffers (about %d bytes per sample on disk):\n", bufferBytesPerSample)
			header = true
		}
		size, maxAge := config.Size(o.BufferMaxBytes).String(), shortDuration(time.Duration(o.BufferMaxAgeSeconds*float64(time.Second)))
		holds := time.Duration(o.BufferHoldsSeconds * float64(time.Second))
		approx := holds.Round(time.Minute)
		if holds >= time.Hour {
			approx = holds.Round(time.Hour)
		}
		switch {
		case holds == 0:
			fmt.Fprintf(out, "  %s: %s, kept up to %s; the outage it holds is known once its jobs have run\n", o.Name, size, maxAge)
		case holds.Seconds() >= o.BufferMaxAgeSeconds:
			fmt.Fprintf(out, "  %s: %s holds about %s of outage at %.0f samples/min; samples older than %s are dropped first\n",
				o.Name, size, shortDuration(approx), o.SamplesPerMinute, maxAge)
		default:
			fmt.Fprintf(out, "  %s: %s holds about %s of outage at %.0f samples/min, less than its maxAge of %s: raise maxSize to keep more\n",
				o.Name, size, shortDuration(approx), o.SamplesPerMinute, maxAge)
		}
	}
}

// wrapList writes items after prefix, separated by commas, wrapped at
// width with the continuation lines aligned under the first item.
func wrapList(prefix string, items []string, width int) string {
	words := slices.Clone(items)
	for i := range len(words) - 1 {
		words[i] += ","
	}
	return wrapWords(prefix, words, width)
}

// wrapWords writes words after a prefix, wrapped at width, the continuation
// lines aligned under the first word.
func wrapWords(prefix string, items []string, width int) string {
	var b strings.Builder
	indent := strings.Repeat(" ", len(prefix))
	line := prefix
	for _, item := range items {
		if len(line) > len(indent) && len(line)+1+len(item) > width {
			b.WriteString(strings.TrimRight(line, " ") + "\n")
			line = indent
		}
		if len(line) > len(indent) && !strings.HasSuffix(line, " ") {
			line += " "
		}
		line += item
	}
	b.WriteString(line + "\n")
	return b.String()
}

func pagesLabel(j planner.JobPlan) string {
	label := strconv.FormatFloat(j.PagesPerRun, 'f', -1, 64) + " pages"
	if j.PagesPerRun == 1 {
		label = "1 page"
	}
	if j.Fanout > 1 {
		label = fmt.Sprintf("%d x %s", j.Fanout, label)
	}
	return label
}

// shortDuration drops the zero trailing units of a duration: 10m, 1h,
// 1h30m, 45s.
func shortDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

type planJSON struct {
	Feasible bool            `json:"feasible"`
	Jobs     []planJobJSON   `json:"jobs"`
	Scopes   []planScopeJSON `json:"scopes"`
	// NotCollected are the jobs that are not collected (ADR 0013).
	NotCollected []planNotCollectedJSON `json:"not_collected"`
	// Outputs are the series estimates of the outputs (ADR 0014).
	Outputs []outputEstimate `json:"outputs"`
}

// outputEstimate is what an output receives, from the series of the last
// successful runs kept in the state store (ADR 0014).
type outputEstimate struct {
	Name             string  `json:"name"`
	Series           int     `json:"series"`
	SamplesPerMinute float64 `json:"samples_per_minute"`
	MaxSeries        int     `json:"max_series,omitempty"`
	// Unmeasured are the jobs sent to the output without a successful run
	// yet: Series is then a lower bound.
	Unmeasured []string `json:"unmeasured_jobs"`
	// The disk buffer of the output (ADR 0015): its maxSize and maxAge, and
	// how long an outage of the backend it holds at SamplesPerMinute
	// (0 when the rate is not known).
	BufferMaxBytes      int64   `json:"buffer_max_bytes,omitempty"`
	BufferMaxAgeSeconds float64 `json:"buffer_max_age_seconds,omitempty"`
	BufferHoldsSeconds  float64 `json:"buffer_holds_seconds,omitempty"`
	// measured counts the jobs with a successful run.
	measured int
}

// bufferBytesPerSample is the space a sample takes in a disk buffer, as an
// order of magnitude: about 20 bytes in large runs, 30 in runs of 50
// samples, more in small ones (measured with gob and snappy), doubled for
// labels less repetitive than the measurement.
const bufferBytesPerSample = 64

// bufferHolds returns how long a buffer of maxBytes holds samples arriving
// at samplesPerMinute, before it drops the oldest; 0 when the rate is not
// known.
func bufferHolds(maxBytes int64, samplesPerMinute float64) time.Duration {
	if samplesPerMinute <= 0 {
		return 0
	}
	minutes := float64(maxBytes) / bufferBytesPerSample / samplesPerMinute
	return time.Duration(minutes * float64(time.Minute))
}

// outputEstimates counts the series every output receives: those of the
// last successful run of every job instance routed to it (kept in the state
// store), after its processing rules; samples per minute divide the series
// of an instance by the effective interval of its job.
func outputEstimates(ctx context.Context, graph *runtime.Graph, store state.Store, res *planner.Result) ([]outputEstimate, error) {
	registry := series.New()
	if store != nil {
		snaps, err := store.LoadSeries(ctx)
		if err != nil {
			return nil, err
		}
		registry.Restore(snaps)
	}
	effective := map[string]time.Duration{}
	for _, j := range res.Jobs {
		effective[j.Name] = j.Effective
	}
	out := []outputEstimate{}
	for _, o := range graph.Outputs {
		name := string(o.Metadata.Name)
		proc, err := processing.Compile(o.Spec.Processing)
		if err != nil {
			return nil, err
		}
		est := outputEstimate{Name: name, Unmeasured: []string{}}
		if o.Spec.Limits != nil {
			est.MaxSeries = o.Spec.Limits.MaxSeries
		}
		routed := collector.RoutedTo(graph, name)
		measured := map[string]bool{}
		for instance, n := range registry.OutputSeriesByInstance(series.Output{Name: name, Processor: proc}, routed) {
			job, _, _ := strings.Cut(instance, "[")
			if !measured[job] {
				est.measured++
			}
			measured[job] = true
			est.Series += n
			interval := effective[job]
			for _, j := range graph.Jobs {
				if j.Name == job && interval == 0 {
					interval = j.Interval
				}
			}
			if interval > 0 {
				est.SamplesPerMinute += float64(n) * time.Minute.Seconds() / interval.Seconds()
			}
		}
		for _, j := range graph.Jobs {
			if routed(j.Name) && !measured[j.Name] && !slices.Contains(est.Unmeasured, j.Name) {
				est.Unmeasured = append(est.Unmeasured, j.Name)
			}
		}
		slices.Sort(est.Unmeasured)
		if q := o.Spec.Queue; q != nil && q.Disk != nil {
			est.BufferMaxBytes, est.BufferMaxAgeSeconds = q.Disk.MaxSize.Bytes(), q.Disk.MaxAge.Std().Seconds()
			est.BufferHoldsSeconds = bufferHolds(est.BufferMaxBytes, est.SamplesPerMinute).Seconds()
		}
		out = append(out, est)
	}
	return out, nil
}

type planNotCollectedJSON struct {
	Name           string  `json:"name"`
	Provider       string  `json:"provider"`
	Account        string  `json:"account"`
	DesiredSeconds float64 `json:"desired_seconds"`
	Reason         string  `json:"reason"`
}

type planJobJSON struct {
	Name             string             `json:"name"`
	Provider         string             `json:"provider"`
	Account          string             `json:"account"`
	Priority         string             `json:"priority"`
	DesiredSeconds   float64            `json:"desired_seconds"`
	EffectiveSeconds float64            `json:"effective_seconds"`
	Stretch          float64            `json:"stretch"`
	Reason           string             `json:"reason,omitempty"`
	CallsPerRun      float64            `json:"calls_per_run"`
	Fanout           int                `json:"fanout"`
	Calls            map[string]float64 `json:"projected_calls"`
}

type planScopeJSON struct {
	ID            string   `json:"id"`
	Limit         string   `json:"limit"`
	Scope         string   `json:"scope"`
	PeriodSeconds float64  `json:"period_seconds"`
	Budget        float64  `json:"budget"`
	Reserve       float64  `json:"reserve"`
	Projected     float64  `json:"projected"`
	Feasible      bool     `json:"feasible"`
	ExhaustionAt  string   `json:"exhaustion_at,omitempty"`
	Jobs          []string `json:"jobs"`
}

func printPlanJSON(e *env, res *planner.Result, notCollected []*runtime.Job, outputs []outputEstimate) error {
	out := planJSON{
		Feasible: res.Feasible, Jobs: []planJobJSON{}, Scopes: []planScopeJSON{}, NotCollected: []planNotCollectedJSON{}, Outputs: outputs,
	}
	for _, j := range notCollected {
		out.NotCollected = append(out.NotCollected, planNotCollectedJSON{
			Name: j.Name, Provider: j.Provider, Account: j.Account, DesiredSeconds: j.Interval.Seconds(), Reason: j.NotCollected,
		})
	}
	for _, j := range res.Jobs {
		out.Jobs = append(out.Jobs, planJobJSON{
			Name: j.Name, Provider: j.Provider, Account: j.Account, Priority: string(j.Priority),
			DesiredSeconds: j.Desired.Seconds(), EffectiveSeconds: j.Effective.Seconds(), Stretch: j.Stretch,
			Reason: j.Describe(), CallsPerRun: j.CallsPerRun, Fanout: j.Fanout, Calls: j.Calls,
		})
	}
	for _, s := range res.Scopes {
		js := planScopeJSON{
			ID: s.ID, Limit: s.Limit, Scope: s.Key, PeriodSeconds: s.Period.Seconds(), Budget: s.Budget,
			Reserve: s.Reserve, Projected: s.Projected, Feasible: s.Feasible, Jobs: s.Jobs,
		}
		if !s.ExhaustionAt.IsZero() {
			js.ExhaustionAt = s.ExhaustionAt.UTC().Format(time.RFC3339)
		}
		out.Scopes = append(out.Scopes, js)
	}
	enc := json.NewEncoder(e.stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}
