package collector

import (
	"context"
	"reflect"

	"github.com/kentrow/tapyr/internal/buildinfo"
	"github.com/kentrow/tapyr/internal/config"
	"github.com/kentrow/tapyr/internal/output"
	"github.com/kentrow/tapyr/internal/output/otlp"
	"github.com/kentrow/tapyr/internal/output/prometheus"
	"github.com/kentrow/tapyr/internal/output/remotewrite"
	"github.com/kentrow/tapyr/internal/processing"
	"github.com/kentrow/tapyr/internal/runtime"
)

// runningOutput is an output and its sender loop.
type runningOutput struct {
	out   output.Output
	spec  config.OutputSpec
	creds *runtime.OutputCredentials
	// dedup tells whether the output deduplicates samples by source
	// timestamp (push outputs).
	dedup bool
	// proc applies the processing rules of the output (nil without).
	proc   *processing.Processor
	cancel context.CancelFunc
	done   chan struct{}
}

// applyOutputs starts the outputs of a graph: an output whose spec did not
// change keeps running (the pull exposition keeps serving), others start
// afresh. Outputs absent from the graph are stopped.
func (c *Collector) applyOutputs(ctx context.Context, graph *runtime.Graph) {
	var stale []string
	next := map[string]*runningOutput{}
	c.omu.RLock()
	for _, o := range graph.Outputs {
		name := string(o.Metadata.Name)
		if prev, ok := c.outputs[name]; ok && reflect.DeepEqual(prev.spec, o.Spec) &&
			sameCredentials(prev.creds, graph.OutputCredentials[name]) {
			next[name] = prev
		}
	}
	for name := range c.outputs {
		if _, kept := next[name]; !kept {
			stale = append(stale, name)
		}
	}
	c.omu.RUnlock()
	c.stopOutputs(ctx, stale)

	c.omu.Lock()
	defer c.omu.Unlock()
	c.prom = nil
	for _, o := range graph.Outputs {
		name := string(o.Metadata.Name)
		ro, ok := next[name]
		if !ok {
			out, err := c.newOutput(name, o.Spec, graph.OutputCredentials[name])
			var proc *processing.Processor
			if err == nil {
				proc, err = processing.Compile(o.Spec.Processing)
			}
			if err != nil {
				// The compiler validated the output: this is not expected.
				c.logger.Error("output not started", "component", "output", "output", name, "error", err)
				continue
			}
			octx, cancel := context.WithCancel(context.WithoutCancel(ctx))
			ro = &runningOutput{
				out: out, spec: o.Spec, creds: graph.OutputCredentials[name], dedup: o.Spec.Type != config.OutputPrometheus,
				proc: proc, cancel: cancel, done: make(chan struct{}),
			}
			go func() {
				defer close(ro.done)
				if err := out.Run(octx); err != nil {
					c.logger.Error("output stopped", "component", "output", "output", name, "error", err)
				}
			}()
			next[name] = ro
		}
		if prom, isProm := ro.out.(*prometheus.Output); isProm {
			c.prom = prom
		}
	}
	c.outputs = next
}

// newOutput builds an output of the configuration.
func (c *Collector) newOutput(name string, spec config.OutputSpec, creds *runtime.OutputCredentials) (output.Output, error) {
	switch spec.Type {
	case config.OutputRemoteWrite:
		return remotewrite.New(remotewrite.Options{
			Name: name, Spec: spec, Credentials: creds, Client: c.client, Clock: c.clock, Logger: c.logger,
			Observer: c.metrics, UserAgent: userAgent(),
		}), nil
	case config.OutputOTLP:
		return otlp.New(otlp.Options{
			Name: name, Spec: spec, Credentials: creds, Client: c.client, Clock: c.clock, Logger: c.logger,
			Observer: c.metrics, Version: buildinfo.Get().Version, InstanceID: c.instance,
		})
	default:
		return prometheus.New(name, spec.Prometheus.ExposeTimestamps, c.logger), nil
	}
}

// userAgent is the User-Agent of the requests of the push outputs.
func userAgent() string { return "tapyr/" + buildinfo.Get().Version }

// sameCredentials reports whether two resolved credentials are equal, so
// that an output whose secrets changed starts afresh on reload.
func sameCredentials(a, b *runtime.OutputCredentials) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.Username.Reveal() != b.Username.Reveal() || a.Password.Reveal() != b.Password.Reveal() || len(a.Headers) != len(b.Headers) {
		return false
	}
	for k, v := range a.Headers {
		if w, ok := b.Headers[k]; !ok || w.Reveal() != v.Reveal() {
			return false
		}
	}
	return true
}

// stopOutputs flushes and stops outputs that are no longer in use.
func (c *Collector) stopOutputs(ctx context.Context, names []string) {
	c.omu.Lock()
	stopping := make([]*runningOutput, 0, len(names))
	for _, name := range names {
		if ro, ok := c.outputs[name]; ok {
			stopping = append(stopping, ro)
			delete(c.outputs, name)
		}
	}
	c.omu.Unlock()
	for _, ro := range stopping {
		if err := ro.out.Flush(ctx); err != nil {
			c.logger.Warn("flushing output", "component", "output", "output", ro.out.Name(), "error", err)
		}
		ro.cancel()
		<-ro.done
	}
}
