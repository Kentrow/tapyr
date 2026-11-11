// Package processing applies the processing rules of an output to the
// samples of the jobs sent to it (ADR 0012, docs/spec/06-mapping-and-outputs.md,
// "Processing"): keep and drop samples, labelkeep and labeldrop labels, with
// the semantics of Prometheus metric_relabel_configs.
package processing

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/kentrow/tapyr/internal/config"
	"github.com/kentrow/tapyr/internal/model"
)

// Processor applies the rules of an output. A nil Processor keeps
// everything unchanged.
type Processor struct {
	rules []rule
}

type rule struct {
	action    config.ProcessingAction
	source    []string
	separator string
	re        *regexp.Regexp
}

// Compile compiles the rules of an output; it returns nil without rules.
func Compile(rules []config.ProcessingRule) (*Processor, error) {
	if len(rules) == 0 {
		return nil, nil
	}
	p := &Processor{rules: make([]rule, 0, len(rules))}
	for i, r := range rules {
		re, err := config.CompileProcessingRegex(r.Regex)
		if err != nil {
			return nil, fmt.Errorf("processing rule %d: %w", i, err)
		}
		sep := ";"
		if r.Separator != nil {
			sep = *r.Separator
		}
		p.rules = append(p.rules, rule{action: r.Action, source: r.SourceLabels, separator: sep, re: re})
	}
	return p, nil
}

// apply returns the series after the rules, and false when a rule drops it.
// The labels are copied before a change: the input is never modified.
func (p *Processor) apply(s model.Series) (model.Series, bool) {
	for i := range p.rules {
		r := &p.rules[i]
		switch r.action {
		case config.ProcessingKeep, config.ProcessingDrop:
			values := make([]string, len(r.source))
			for j, name := range r.source {
				if name == "__name__" {
					values[j] = s.Name
				} else {
					values[j] = s.Labels.Get(name)
				}
			}
			if r.re.MatchString(strings.Join(values, r.separator)) != (r.action == config.ProcessingKeep) {
				return model.Series{}, false
			}
		case config.ProcessingLabelKeep, config.ProcessingLabelDrop:
			keep := r.action == config.ProcessingLabelKeep
			if !slices.ContainsFunc(s.Labels, func(l model.Label) bool { return r.re.MatchString(l.Name) != keep }) {
				continue
			}
			labels := make(model.Labels, 0, len(s.Labels))
			for _, l := range s.Labels {
				if r.re.MatchString(l.Name) == keep {
					labels = append(labels, l)
				}
			}
			s.Labels = labels
		}
	}
	return s, true
}

// Samples applies the rules to the samples of a run. It returns the samples
// kept, in order, the number filtered by keep and drop, and the number of
// duplicates: samples that end up with the series and timestamp of an
// earlier one (a labeldrop removing the label that told them apart), which
// are dropped.
func (p *Processor) Samples(in []model.Sample) (out []model.Sample, filtered, duplicates int) {
	if p == nil {
		return in, 0, 0
	}
	out = make([]model.Sample, 0, len(in))
	seen := make(map[string]bool, len(in))
	for i := range in {
		s := in[i]
		series, ok := p.apply(s.Series())
		if !ok {
			filtered++
			continue
		}
		s.Labels = series.Labels
		key := series.Key() + "\x00" + s.Timestamp.UTC().Format("2006-01-02T15:04:05.999999999")
		if seen[key] {
			duplicates++
			continue
		}
		seen[key] = true
		out = append(out, s)
	}
	return out, filtered, duplicates
}

// Series applies the rules to series and returns those kept, by key: two
// series merged by a labeldrop are one.
func (p *Processor) Series(in []model.Series) map[string]model.Series {
	out := make(map[string]model.Series, len(in))
	for _, s := range in {
		if p != nil {
			var ok bool
			if s, ok = p.apply(s); !ok {
				continue
			}
		}
		if key := s.Key(); out[key].Name == "" {
			out[key] = s
		}
	}
	return out
}
