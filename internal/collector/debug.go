package collector

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/kentrow/tapyr/internal/api"
	"github.com/kentrow/tapyr/internal/collect"
	"github.com/kentrow/tapyr/internal/config"
	"github.com/kentrow/tapyr/internal/model"
	"github.com/kentrow/tapyr/internal/runtime"
)

// maxDebugCaptures bounds the instances whose last run is kept by the live
// debug endpoint: the oldest captures are dropped first.
const maxDebugCaptures = 200

// debugCaptures keeps the last run of the job instances for the live debug
// endpoint, in memory only.
type debugCaptures struct {
	redact func(string) string

	mu       sync.Mutex
	enabled  bool
	maxBody  int
	captures map[string]*api.Debug // by instance
	order    []string              // instances, oldest capture first
}

func newDebugCaptures(redact func(string) string) *debugCaptures {
	return &debugCaptures{redact: redact, captures: map[string]*api.Debug{}}
}

// configure applies the debug settings of a configuration; disabling the
// endpoint drops the captures.
func (d *debugCaptures) configure(enabled bool, maxBody int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.enabled, d.maxBody = enabled, maxBody
	if !enabled {
		d.captures, d.order = map[string]*api.Debug{}, nil
	}
}

// debugFailure is what the live debug shows of a run that did not
// succeed: its problem in words, and the request that failed.
type debugFailure struct {
	problem *api.Problem
	request *collect.Error
}

// keep records the last run of an instance: its pages (redacted bodies,
// truncated, with the headers of interest), the response that failed, and
// its samples.
func (d *debugCaptures) keep(job *runtime.Job, runID string, at time.Time, result, errMsg string,
	res *collect.Result, samples []model.Sample, headers []string, failure debugFailure,
) {
	d.mu.Lock()
	enabled, maxBody := d.enabled, d.maxBody
	d.mu.Unlock()
	if !enabled {
		return
	}
	c := &api.Debug{
		Job: job.Name, Instance: job.Instance, RunID: runID, Time: at.UTC(), Result: result, Error: d.redact(errMsg),
		Problem: failure.problem, Pages: []api.DebugPage{}, Samples: make([]api.DebugSample, 0, len(samples)),
	}
	page := func(status int, header http.Header, body string) api.DebugPage {
		p := api.DebugPage{Status: status, Headers: map[string]string{}}
		for _, h := range headers {
			if v := header.Get(h); v != "" {
				p.Headers[h] = v
			}
		}
		text := d.redact(body)
		if maxBody > 0 && len(text) > maxBody {
			text, p.Truncated = text[:maxBody], true
		}
		p.Body = text
		return p
	}
	if res != nil {
		for _, p := range res.Pages {
			body, err := json.MarshalIndent(p.Body, "", "  ")
			if err != nil {
				body = []byte("<unprintable body>")
			}
			c.Pages = append(c.Pages, page(p.Status, p.Headers, string(body)))
		}
	}
	// The response that failed (an error status, a body that does not
	// decode) is not a page of the result: shown as received, redacted.
	if f := failure.request; f != nil && f.Status != 0 {
		c.Pages = append(c.Pages, page(f.Status, f.Header, indentJSON(f.Body)))
	}
	for _, s := range samples {
		labels := make(map[string]string, len(s.Labels))
		for _, l := range s.Labels {
			labels[l.Name] = d.redact(l.Value)
		}
		c.Samples = append(c.Samples, api.DebugSample{Name: s.Name, Type: s.Type.String(), Labels: labels, Value: s.Value})
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.enabled {
		return
	}
	if _, ok := d.captures[job.Instance]; ok {
		d.order = slices.DeleteFunc(d.order, func(i string) bool { return i == job.Instance })
	}
	d.captures[job.Instance] = c
	d.order = append(d.order, job.Instance)
	for len(d.order) > maxDebugCaptures {
		delete(d.captures, d.order[0])
		d.order = d.order[1:]
	}
}

// remove drops the capture of a removed instance.
func (d *debugCaptures) remove(instance string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.captures[instance]; ok {
		delete(d.captures, instance)
		d.order = slices.DeleteFunc(d.order, func(i string) bool { return i == instance })
	}
}

// get returns the capture of an instance of a job, or the most recent one
// of the job when instance is empty.
func (d *debugCaptures) get(_ context.Context, job, instance string) (api.Debug, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.enabled {
		return api.Debug{}, api.ErrDebugDisabled
	}
	var instances []string
	var latest *api.Debug
	for _, i := range d.order {
		if c := d.captures[i]; c.Job == job {
			instances = append(instances, i)
			if instance == "" || i == instance {
				latest = c
			}
		}
	}
	if latest == nil {
		return api.Debug{}, api.ErrNotFound
	}
	out := *latest
	slices.Sort(instances)
	out.Instances = instances
	return out, nil
}

// debugHeaders are the response headers shown by the live debug endpoint,
// besides the quota headers of the provider.
var debugHeaders = []string{"Content-Type", "Link", "X-Next-Cursor", http.CanonicalHeaderKey("Retry-After")}

// indentJSON indents a JSON body for reading, and returns any other body
// as it is.
func indentJSON(body []byte) string {
	var out bytes.Buffer
	if json.Indent(&out, body, "", "  ") == nil {
		return out.String()
	}
	return string(body)
}

// KeptHeaders returns the response headers recorded per provider: the
// quota headers of its limits and the rate-limit header, never others.
func KeptHeaders(providers []*config.Provider) map[string][]string {
	out := map[string][]string{}
	for _, p := range providers {
		set := map[string]bool{"Retry-After": true}
		if e := p.Spec.Errors; e != nil && e.RateLimited != nil && e.RateLimited.RetryAfterHeader != "" {
			set[http.CanonicalHeaderKey(e.RateLimited.RetryAfterHeader)] = true
		}
		for _, l := range p.Spec.Limits {
			if s := l.Sync; s != nil {
				for _, h := range []string{s.LimitHeader, s.RemainingHeader, s.ResetHeader} {
					if h != "" {
						set[http.CanonicalHeaderKey(h)] = true
					}
				}
			}
		}
		names := make([]string, 0, len(set))
		for h := range set {
			names = append(names, h)
		}
		slices.Sort(names)
		out[string(p.Metadata.Name)] = names
	}
	return out
}

// TLSClient returns the HTTP client of an account with TLS settings. The
// executor makes every client refuse to follow redirects by itself.
func TLSClient(t *runtime.AccountTLS) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = t.Config
	return &http.Client{Transport: transport}
}
