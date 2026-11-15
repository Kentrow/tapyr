package runtime

import (
	"slices"
	"strings"

	"github.com/kentrow/tapyr/internal/config"
	"github.com/kentrow/tapyr/internal/processing"
)

// Reasons for which a job is not collected (ADR 0013).
const (
	ReasonNoOutputKeeps  = "no output keeps its metrics"
	ReasonNoInventoryUse = "no collected job takes its inventory"
)

// splitCollected moves the jobs that are not collected from the graph to
// NotCollected (ADR 0013): a job is collected when one of its metrics can
// reach an output (an output receiving it whose processing rules may keep
// the metric, judged by its name), or when a collected job takes its
// inventory (forEach). Without any output, every job is collected.
func (c *compiler) splitCollected() {
	g := c.graph
	if len(g.Outputs) == 0 || g.Settings.Collection.SkipUnconsumed == nil || !*g.Settings.Collection.SkipUnconsumed {
		return
	}
	processors := map[string]*processing.Processor{}
	for _, o := range g.Outputs {
		p, err := processing.Compile(o.Spec.Processing)
		if err != nil {
			// Validated with the configuration: keep every job on an
			// unexpected error rather than drop one on it.
			return
		}
		processors[string(o.Metadata.Name)] = p
	}
	// The models of a job (one per target) share its definition: decide
	// per name.
	byName := map[string]*Job{}
	children := map[string][]string{}
	for _, j := range g.Jobs {
		if _, ok := byName[j.Name]; !ok {
			byName[j.Name] = j
			if j.ForEach != nil && j.ForEach.ParentJob != "" {
				children[j.ForEach.ParentJob] = append(children[j.ForEach.ParentJob], j.Name)
			}
		}
	}
	emits := func(j *Job) bool {
		if j.Role == config.RoleInventory {
			return false
		}
		names := j.Extract.MetricNames()
		for _, o := range g.Outputs {
			name := string(o.Metadata.Name)
			if len(j.Outputs) > 0 && !slices.Contains(j.Outputs, name) {
				continue
			}
			if slices.ContainsFunc(names, processors[name].MayKeep) {
				return true
			}
		}
		return false
	}
	collected := map[string]bool{}
	var visit func(name string) bool
	visit = func(name string) bool {
		if v, ok := collected[name]; ok {
			return v
		}
		collected[name] = false // forEach chains have no cycle (validated)
		v := emits(byName[name])
		for _, child := range children[name] {
			v = visit(child) || v
		}
		collected[name] = v
		return v
	}
	var kept []*Job
	for _, j := range g.Jobs {
		if visit(j.Name) {
			kept = append(kept, j)
			continue
		}
		j.NotCollected = ReasonNoOutputKeeps
		if j.Role == config.RoleInventory || (len(children[j.Name]) > 0 && j.Extract.MetricNames() == nil) {
			j.NotCollected = ReasonNoInventoryUse
		}
		g.NotCollected = append(g.NotCollected, j)
	}
	g.Jobs = kept
}

// NotCollectedJobs returns one model per job that is not collected (the
// first target of a multi-target account), sorted by name.
func (g *Graph) NotCollectedJobs() []*Job {
	var out []*Job
	seen := map[string]bool{}
	for _, j := range g.NotCollected {
		if !seen[j.Name] {
			seen[j.Name] = true
			out = append(out, j)
		}
	}
	slices.SortFunc(out, func(a, b *Job) int { return strings.Compare(a.Name, b.Name) })
	return out
}
