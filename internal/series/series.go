// Package series is the series registry (see
// docs/spec/06-mapping-and-outputs.md, "Series registry"): per job instance,
// the series of the last successful run, the processed series last sent to
// every output with processing rules, and the last source timestamp sent per
// series and output. It drives staleness and deduplication.
package series

import (
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/kentrow/tapyr/internal/model"
	"github.com/kentrow/tapyr/internal/processing"
	"github.com/kentrow/tapyr/internal/state"
)

// Output is an output receiving the samples of a run.
type Output struct {
	Name string
	// Dedup drops samples whose source timestamp is not after the last one
	// sent (push outputs). The pull output exposes the latest value and
	// receives every sample.
	Dedup bool
	// Processor applies the processing rules of the output (nil without).
	Processor *processing.Processor
}

// Delivery is what an output receives for a successful run.
type Delivery struct {
	Samples []model.Sample
	// Stale are the series of the previous successful run absent from this one.
	Stale []model.Series
	// Deduplicated counts the samples dropped by deduplication.
	Deduplicated int
	// Filtered counts the samples removed by the keep and drop rules, and
	// Duplicates those dropped because the rules made them collide.
	Filtered, Duplicates int
}

// Withdrawal is what a withdrawn instance leaves: the series to mark stale.
type Withdrawal struct {
	// Series are the series of its last successful run.
	Series []model.Series
	// Outputs are the processed series last sent to the outputs with
	// processing rules.
	Outputs map[string][]model.Series
}

// For returns the series withdrawn from an output: the processed series
// last sent to it, or, when the registry does not know them (after a
// restart), the series of the last run processed with p.
func (w Withdrawal) For(output string, p *processing.Processor) []model.Series {
	if s, ok := w.Outputs[output]; ok {
		return s
	}
	if p == nil {
		return w.Series
	}
	return sortedSeries(p.Series(w.Series))
}

// Registry tracks the series of every job instance. It is safe for
// concurrent use.
type Registry struct {
	mu        sync.Mutex
	instances map[string]*instance
	// dirty and removed are the instances changed and removed since the
	// last snapshot.
	dirty   map[string]bool
	removed map[string]bool
}

type instance struct {
	lastSuccess time.Time
	series      map[string]model.Series
	// processed are the processed series last sent to the outputs with
	// processing rules, by key; not persisted, derived from series after a
	// restart.
	processed map[string]map[string]model.Series // output -> series key -> series
	sent      map[string]map[string]time.Time    // output -> series key -> source timestamp
}

// New returns an empty registry.
func New() *Registry {
	return &Registry{instances: map[string]*instance{}, dirty: map[string]bool{}, removed: map[string]bool{}}
}

// Success records the samples of a successful run of an instance and
// returns, per output, the samples to send and the series now stale.
func (r *Registry) Success(name string, samples []model.Sample, outputs []Output, at time.Time) map[string]Delivery {
	current := map[string]model.Series{}
	for i := range samples {
		s := samples[i].Series()
		if key := s.Key(); !hasKey(current, key) {
			current[key] = s
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	inst := r.instance(name)
	var stale []model.Series
	for _, key := range slices.Sorted(maps.Keys(inst.series)) {
		if _, ok := current[key]; !ok {
			stale = append(stale, inst.series[key])
		}
	}
	out := make(map[string]Delivery, len(outputs))
	// alive are the series an output now receives, when they differ from
	// the series of the run (outputs with processing rules).
	alive := map[string]map[string]model.Series{}
	for _, o := range outputs {
		d := Delivery{Samples: samples, Stale: stale}
		prev, known := inst.processed[o.Name]
		if o.Processor != nil || known {
			var now map[string]model.Series
			d.Samples, d.Filtered, d.Duplicates = o.Processor.Samples(samples)
			if o.Processor != nil {
				now = seriesOf(d.Samples)
				inst.processed[o.Name] = now
			} else {
				now = current // the rules were removed: back to the raw series
				delete(inst.processed, o.Name)
			}
			if !known {
				prev = o.Processor.Series(slices.Collect(maps.Values(inst.series)))
			}
			d.Stale = nil
			for _, key := range slices.Sorted(maps.Keys(prev)) {
				if _, ok := now[key]; !ok {
					d.Stale = append(d.Stale, prev[key])
				}
			}
			alive[o.Name] = now
		}
		if o.Dedup {
			d.Samples, d.Deduplicated = inst.dedup(o.Name, d.Samples)
		}
		out[o.Name] = d
	}
	// The timestamps of stale series are forgotten, which bounds the
	// registry to the series alive.
	for output, sent := range inst.sent {
		live, ok := alive[output]
		if !ok {
			live = current
		}
		for key := range sent {
			if _, ok := live[key]; !ok {
				delete(sent, key)
			}
		}
	}
	inst.series = current
	inst.lastSuccess = at
	r.dirty[name] = true
	return out
}

// dedup drops the samples with a source timestamp not after the last one
// sent to the output, and records the timestamps sent.
func (inst *instance) dedup(output string, samples []model.Sample) ([]model.Sample, int) {
	sent := inst.sent[output]
	if sent == nil {
		sent = map[string]time.Time{}
		inst.sent[output] = sent
	}
	kept := make([]model.Sample, 0, len(samples))
	dropped := 0
	for i := range samples {
		s := &samples[i]
		if !s.SourceTimestamp {
			kept = append(kept, *s)
			continue
		}
		key := s.SeriesKey()
		if last, ok := sent[key]; ok && !s.Timestamp.After(last) {
			dropped++
			continue
		}
		sent[key] = s.Timestamp
		kept = append(kept, *s)
	}
	return kept, dropped
}

// Expired returns the instances to withdraw at now, sorted: those with
// series whose last success is older than their staleAfter, and those
// unknown to staleAfter (ok false: removed from the configuration).
func (r *Registry) Expired(now time.Time, staleAfter func(instance string) (time.Duration, bool)) []string {
	r.mu.Lock()
	names := slices.Sorted(maps.Keys(r.instances))
	last := make([]time.Time, len(names))
	empty := make([]bool, len(names))
	for i, name := range names {
		last[i], empty[i] = r.instances[name].lastSuccess, len(r.instances[name].series) == 0
	}
	r.mu.Unlock()
	var out []string
	for i, name := range names {
		d, ok := staleAfter(name)
		if !ok || (!empty[i] && now.Sub(last[i]) > d) {
			out = append(out, name)
		}
	}
	return out
}

// Expire withdraws the series of an instance whose runs keep failing and
// returns them. The timestamps sent are kept for when the runs recover.
func (r *Registry) Expire(name string) Withdrawal {
	r.mu.Lock()
	defer r.mu.Unlock()
	inst, ok := r.instances[name]
	if !ok {
		return Withdrawal{}
	}
	w := inst.withdrawal()
	inst.series = map[string]model.Series{}
	clear(inst.processed)
	r.dirty[name] = true
	return w
}

// Remove forgets an instance (removed by discovery or the configuration)
// and returns its series, now stale.
func (r *Registry) Remove(name string) Withdrawal {
	r.mu.Lock()
	defer r.mu.Unlock()
	inst, ok := r.instances[name]
	if !ok {
		return Withdrawal{}
	}
	delete(r.instances, name)
	delete(r.dirty, name)
	r.removed[name] = true
	return inst.withdrawal()
}

// withdrawal returns the series of the instance and of its outputs.
func (inst *instance) withdrawal() Withdrawal {
	w := Withdrawal{Series: sortedSeries(inst.series)}
	for output, series := range inst.processed {
		if w.Outputs == nil {
			w.Outputs = map[string][]model.Series{}
		}
		w.Outputs[output] = sortedSeries(series)
	}
	return w
}

// Snapshot returns the state of the instances changed since the last
// snapshot, and the instances removed.
func (r *Registry) Snapshot() (changed []state.SeriesSnapshot, removed []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, name := range slices.Sorted(maps.Keys(r.dirty)) {
		inst := r.instances[name]
		snap := state.SeriesSnapshot{
			Instance: name, LastSuccess: inst.lastSuccess, Series: slices.Sorted(maps.Keys(inst.series)),
			Sent: make(map[string]map[string]time.Time, len(inst.sent)),
		}
		for output, sent := range inst.sent {
			snap.Sent[output] = maps.Clone(sent)
		}
		changed = append(changed, snap)
	}
	removed = slices.Sorted(maps.Keys(r.removed))
	clear(r.dirty)
	clear(r.removed)
	return changed, removed
}

// Restore loads snapshots, for example after a restart. Series keys that
// cannot be parsed are skipped and counted.
func (r *Registry) Restore(snaps []state.SeriesSnapshot) (skipped int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, snap := range snaps {
		inst := r.instance(snap.Instance)
		inst.lastSuccess = snap.LastSuccess
		for _, key := range snap.Series {
			s, err := model.ParseSeriesKey(key)
			if err != nil {
				skipped++
				continue
			}
			inst.series[key] = s
		}
		for output, sent := range snap.Sent {
			inst.sent[output] = maps.Clone(sent)
		}
	}
	return skipped
}

// Series returns the series of the last successful run of an instance.
func (r *Registry) Series(name string) []model.Series {
	r.mu.Lock()
	defer r.mu.Unlock()
	if inst, ok := r.instances[name]; ok {
		return sortedSeries(inst.series)
	}
	return nil
}

// LastSuccess returns the time of the last successful run of an instance;
// ok is false when it never succeeded.
func (r *Registry) LastSuccess(name string) (at time.Time, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if inst, found := r.instances[name]; found && !inst.lastSuccess.IsZero() {
		return inst.lastSuccess, true
	}
	return time.Time{}, false
}

// instance returns the state of an instance, created if needed; r.mu must
// be held.
func (r *Registry) instance(name string) *instance {
	inst, ok := r.instances[name]
	if !ok {
		inst = &instance{
			series: map[string]model.Series{}, processed: map[string]map[string]model.Series{},
			sent: map[string]map[string]time.Time{},
		}
		r.instances[name] = inst
		delete(r.removed, name)
	}
	return inst
}

// seriesOf returns the series of samples, by key.
func seriesOf(samples []model.Sample) map[string]model.Series {
	out := make(map[string]model.Series, len(samples))
	for i := range samples {
		s := samples[i].Series()
		if key := s.Key(); !hasKey(out, key) {
			out[key] = s
		}
	}
	return out
}

func hasKey(m map[string]model.Series, key string) bool {
	_, ok := m[key]
	return ok
}

func sortedSeries(m map[string]model.Series) []model.Series {
	out := make([]model.Series, 0, len(m))
	for _, key := range slices.Sorted(maps.Keys(m)) {
		out = append(out, m[key])
	}
	return out
}
