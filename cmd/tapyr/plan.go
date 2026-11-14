package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/kentrow/tapyr/internal/budget"
	"github.com/kentrow/tapyr/internal/budget/planner"
	"github.com/kentrow/tapyr/internal/collector"
	"github.com/kentrow/tapyr/internal/config"
	"github.com/kentrow/tapyr/internal/runtime"
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
			if format == "json" {
				err = printPlanJSON(e, res, notCollected)
			} else {
				err = printPlanTable(e, res, notCollected)
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

func printPlanTable(e *env, res *planner.Result, notCollected []*runtime.Job) error {
	w := tabwriter.NewWriter(e.stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "QUOTA SCOPE\tWINDOW\tBUDGET\tPROJECTED\tRESERVE\tSTATUS")
	planned := map[string]bool{}
	for _, s := range res.Scopes {
		status := "ok"
		if !s.Feasible {
			status = "INFEASIBLE"
		}
		if s.Budget > 0 {
			status += fmt.Sprintf(" (%.1f%%)", 100*s.Projected/s.Budget)
		}
		fmt.Fprintf(w, "%s\t%s\t%.0f\t%.0f\t%.0f\t%s\n", s.Limit, shortDuration(s.Period), s.Budget, s.Projected, s.Reserve, status)
		fmt.Fprintf(w, "  %s\t\t\t\t\t\n", s.Key)
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
				line += fmt.Sprintf("\t(stretched x%.2f, priority %s, %s)", j.Stretch, j.Priority, j.Describe())
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
	if len(notCollected) > 0 {
		fmt.Fprintln(w, "\nJOBS NOT COLLECTED\t\t\t\t\t")
		for _, j := range notCollected {
			fmt.Fprintf(w, "  job %s\tdesired %s\t%s\t\t\t\n", j.Name, shortDuration(j.Interval), j.NotCollected)
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if !res.Feasible {
		fmt.Fprintln(e.stdout, "\nThe plan is infeasible: critical jobs alone exceed a quota, or stretching every other job to the maximum is not enough.")
	}
	return nil
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

func printPlanJSON(e *env, res *planner.Result, notCollected []*runtime.Job) error {
	out := planJSON{Feasible: res.Feasible, Jobs: []planJobJSON{}, Scopes: []planScopeJSON{}, NotCollected: []planNotCollectedJSON{}}
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
