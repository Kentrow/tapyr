package collector

import (
	"context"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	dto "github.com/prometheus/client_model/go"

	"github.com/kentrow/tapyr/internal/config"
	"github.com/kentrow/tapyr/internal/model"
)

// selfInstance is the pseudo job instance of the pushed self-metrics.
const selfInstance = "tapyr_self"

// selfPushLoop pushes the self-metrics to the outputs of
// settings.selfMetrics.outputs every selfMetrics.interval until ctx is done.
// Without outputs it waits for a reload, on no timer.
func (c *Collector) selfPushLoop(ctx context.Context) {
	for {
		c.gmu.Lock()
		names, interval, changed := c.settings.SelfMetrics.Outputs, c.settings.SelfMetrics.Interval.Std(), c.selfChanged
		c.gmu.Unlock()
		if len(names) == 0 || interval <= 0 {
			select {
			case <-ctx.Done():
				return
			case <-changed:
				continue
			}
		}
		timer := c.clock.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-changed:
			timer.Stop()
			continue
		case <-timer.C():
		}
		c.pushSelf(names)
	}
}

// pushSelf enqueues the current self-metrics in the given outputs.
func (c *Collector) pushSelf(names []config.Name) {
	families, err := c.metrics.Gather()
	if err != nil {
		c.logger.Warn("gathering the self-metrics", "component", "selfobs", "error", err)
	}
	now := c.clock.Now()
	b := model.Batch{Instance: selfInstance, Job: selfInstance, At: now, Samples: selfSamples(families, now)}
	c.omu.RLock()
	defer c.omu.RUnlock()
	for _, name := range names {
		if ro, ok := c.outputs[string(name)]; ok {
			ro.out.Enqueue(b)
		}
	}
}

// selfSamples converts metric families into samples: counters and gauges as
// such, histograms as their _bucket, _sum and _count counters. Summaries
// are skipped (Tapyr exposes none besides the Go collector's).
func selfSamples(families []*dto.MetricFamily, now time.Time) []model.Sample {
	var out []model.Sample
	for _, mf := range families {
		name, help := mf.GetName(), mf.GetHelp()
		for _, m := range mf.GetMetric() {
			labels := make(model.Labels, 0, len(m.GetLabel())+1)
			for _, l := range m.GetLabel() {
				if l.GetValue() != "" {
					labels = append(labels, model.Label{Name: l.GetName(), Value: l.GetValue()})
				}
			}
			add := func(n string, typ model.MetricType, ls model.Labels, v float64) {
				out = append(out, model.Sample{Name: n, Type: typ, Help: help, Labels: ls, Value: v, Timestamp: now})
			}
			switch mf.GetType() {
			case dto.MetricType_COUNTER:
				add(name, model.Counter, labels, m.GetCounter().GetValue())
			case dto.MetricType_GAUGE:
				add(name, model.Gauge, labels, m.GetGauge().GetValue())
			case dto.MetricType_UNTYPED:
				add(name, model.Gauge, labels, m.GetUntyped().GetValue())
			case dto.MetricType_HISTOGRAM:
				h := m.GetHistogram()
				for _, b := range h.GetBucket() {
					if math.IsInf(b.GetUpperBound(), +1) {
						continue // added below
					}
					add(name+"_bucket", model.Counter, withLabel(labels, "le", formatBound(b.GetUpperBound())), float64(b.GetCumulativeCount()))
				}
				add(name+"_bucket", model.Counter, withLabel(labels, "le", "+Inf"), float64(h.GetSampleCount()))
				add(name+"_sum", model.Counter, labels, h.GetSampleSum())
				add(name+"_count", model.Counter, labels, float64(h.GetSampleCount()))
			}
		}
	}
	return out
}

// withLabel returns ls with one more label, sorted.
func withLabel(ls model.Labels, name, value string) model.Labels {
	out := append(slices.Clone(ls), model.Label{Name: name, Value: value})
	slices.SortFunc(out, func(a, b model.Label) int { return strings.Compare(a.Name, b.Name) })
	return out
}

func formatBound(f float64) string { return strconv.FormatFloat(f, 'g', -1, 64) }
