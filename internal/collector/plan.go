package collector

import (
	"context"
	"slices"

	"github.com/kentrow/tapyr/internal/budget"
	"github.com/kentrow/tapyr/internal/budget/planner"
	"github.com/kentrow/tapyr/internal/config"
	"github.com/kentrow/tapyr/internal/runtime"
	"github.com/kentrow/tapyr/internal/scheduler"
	"github.com/kentrow/tapyr/internal/state"
)

// historyRuns is the number of recent runs the estimates are based on.
const historyRuns = 20

// PlanEntry is a job to plan: a configured instance, or the model of a
// fan-out with its number of instances.
type PlanEntry struct {
	Job    *runtime.Job
	Fanout int
}

// PlanInputs builds the planner inputs, one per scheduling group: scopes
// from the engine, estimates from the run history (median pages of the last
// runs, retry ratio), else from estimate.pagesPerRun.
func PlanInputs(ctx context.Context, entries []PlanEntry, engine *budget.Engine, store state.Store) ([]planner.Job, error) {
	jobs := make([]planner.Job, 0, len(entries))
	for _, e := range entries {
		j := e.Job
		pj := planner.Job{
			Name: scheduler.Group(j), Provider: j.Provider, Account: j.Account, Priority: j.Priority, Desired: j.Interval,
			PagesPerRun: 1, RetryOverhead: planner.DefaultRetryOverhead, Fanout: max(1, e.Fanout),
		}
		if j.PagesEstimate > 0 {
			pj.PagesPerRun = float64(j.PagesEstimate)
		}
		var fanoutVars []string
		if j.ForEach != nil {
			// Variables of the fan-out and of its parents are per instance:
			// every path variable not set by a param.
			for _, v := range config.PathVars(j.Request.Path) {
				if _, isParam := j.Vars[v]; !isParam {
					fanoutVars = append(fanoutVars, v)
				}
			}
			fanoutVars = append(fanoutVars, j.ForEach.Vars...)
		}
		if store != nil {
			var runs []state.RunRecord
			var err error
			if j.ForEach != nil {
				runs, err = store.RunsOfJob(ctx, j.Name, historyRuns)
			} else {
				runs, err = store.Runs(ctx, j.Name, j.Instance, historyRuns)
			}
			if err != nil {
				return nil, err
			}
			if pages, overhead, ok := estimates(runs); ok {
				pj.PagesPerRun, pj.RetryOverhead = pages, overhead
			}
		}
		pj.Scopes = engine.PlanScopes(budget.Descriptor{
			Provider: j.Provider, Account: j.Account, Target: j.Target, Credential: j.Credential, Endpoint: j.Request.Path,
			Method: j.Request.Method, Vars: j.Vars, Job: j.Name, Priority: j.Priority,
		}, fanoutVars)
		jobs = append(jobs, pj)
	}
	return jobs, nil
}

// estimates returns the median pages of successful runs and the retry
// ratio of the history.
func estimates(runs []state.RunRecord) (pages, overhead float64, ok bool) {
	var counts []int
	var requests, retries int
	for _, r := range runs {
		if r.Result != "success" {
			continue
		}
		counts = append(counts, r.Pages)
		requests += r.Requests
		retries += r.Retries
	}
	if len(counts) == 0 {
		return 0, 0, false
	}
	slices.Sort(counts)
	mid := len(counts) / 2
	median := float64(counts[mid])
	if len(counts)%2 == 0 {
		median = float64(counts[mid-1]+counts[mid]) / 2
	}
	overhead = planner.DefaultRetryOverhead
	if requests > retries {
		overhead = float64(retries) / float64(requests-retries)
	}
	return median, overhead, true
}
