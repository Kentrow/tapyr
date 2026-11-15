package mapping

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"cel.dev/cel-go/common/types/ref"

	"github.com/kentrow/tapyr/internal/config"
	"github.com/kentrow/tapyr/internal/model"
)

// FieldError is a compilation error located at a path relative to the
// compiled block (for example "metrics", "0", "value").
type FieldError struct {
	Path []string
	Err  error
}

// Extract is a compiled extract block.
type Extract struct {
	items   *Program
	filter  *Program
	metrics []*metric
	info    []*metric
	post    []*metric
	// inventory
	key    *Program
	fields map[string]*Program
}

// MetricNames returns the names of the metrics the extraction can produce
// (metrics, info and post), sorted and without duplicates; nil for a nil
// Extract.
func (x *Extract) MetricNames() []string {
	if x == nil {
		return nil
	}
	var names []string
	for _, group := range [][]*metric{x.metrics, x.info, x.post} {
		for _, m := range group {
			names = append(names, m.name)
		}
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// InventoryItem is an item produced by an inventory job.
type InventoryItem struct {
	Key    string
	Fields map[string]any
}

type metric struct {
	path      string // for error messages: metrics[0]
	name      string
	typ       model.MetricType
	help      string
	unit      string
	value     *Program // nil for info metrics (value 1)
	labels    []labelProgram
	timestamp *Program
	skipIf    *Program
	// accumulate marks window values; window identifies their window.
	accumulate bool
	window     *Program
}

type labelProgram struct {
	name string
	prg  *Program
}

// CompileExtract compiles an extract block. A nil block compiles to an
// extract producing no samples.
func (e *Env) CompileExtract(x *config.Extract) (*Extract, []FieldError) {
	out := &Extract{}
	if x == nil {
		return out, nil
	}
	var errs []FieldError
	compile := func(ctx Context, src config.Expr, want Output, path ...string) *Program {
		if src == "" {
			return nil
		}
		p, err := e.Compile(ctx, string(src), want)
		if err != nil {
			errs = append(errs, FieldError{Path: path, Err: err})
		}
		return p
	}
	out.items = compile(ContextPage, x.Items, OutputList, "items")
	out.filter = compile(ContextItem, x.Filter, OutputBool, "filter")

	compileMetric := func(ctx Context, block string, i int, m *config.Metric) *metric {
		idx := strconv.Itoa(i)
		cm := &metric{
			path: fmt.Sprintf("%s[%d]", block, i), name: m.Name, help: m.Help, unit: m.Unit,
			typ:       model.Gauge,
			value:     compile(ctx, m.Value, OutputNumber, block, idx, "value"),
			timestamp: compile(ctx, m.Timestamp, OutputTimestamp, block, idx, "timestamp"),
			skipIf:    compile(ctx, m.SkipIf, OutputBool, block, idx, "skipIf"),
		}
		if m.Type == config.MetricCounter {
			cm.typ = model.Counter
		}
		cm.accumulate = m.Accumulate
		cm.window = compile(ctx, m.WindowField, OutputAny, block, idx, "windowField")
		cm.labels = compileLabels(m.Labels, func(name string, src config.Expr) *Program {
			return compile(ctx, src, OutputString, block, idx, "labels", name)
		})
		return cm
	}
	for i := range x.Metrics {
		out.metrics = append(out.metrics, compileMetric(ContextItem, "metrics", i, &x.Metrics[i]))
	}
	for i := range x.Post {
		out.post = append(out.post, compileMetric(ContextPost, "post", i, &x.Post[i]))
	}
	for i, m := range x.Info {
		idx := strconv.Itoa(i)
		out.info = append(out.info, &metric{
			path: fmt.Sprintf("info[%d]", i), name: m.Name, help: m.Help, typ: model.Gauge,
			skipIf: compile(ContextItem, m.SkipIf, OutputBool, "info", idx, "skipIf"),
			labels: compileLabels(m.Labels, func(name string, src config.Expr) *Program {
				return compile(ContextItem, src, OutputString, "info", idx, "labels", name)
			}),
		})
	}
	if inv := x.Inventory; inv != nil {
		out.key = compile(ContextItem, inv.Key, OutputString, "inventory", "key")
		out.fields = map[string]*Program{}
		for _, k := range sortedKeys(inv.Fields) {
			out.fields[k] = compile(ContextItem, inv.Fields[k], OutputAny, "inventory", "fields", k)
		}
	}
	return out, errs
}

func compileLabels(labels map[string]config.Expr, compile func(string, config.Expr) *Program) []labelProgram {
	out := make([]labelProgram, 0, len(labels))
	for _, name := range sortedKeys(labels) {
		out = append(out, labelProgram{name: name, prg: compile(name, labels[name])})
	}
	return out
}

// Page is one HTTP response of a run.
type Page struct {
	Status  int
	Headers http.Header
	// Body is the JSON body decoded by DecodeJSON.
	Body       any
	ReceivedAt time.Time
}

// Value returns the page as the CEL response variable.
func (p *Page) Value() map[string]any { return p.value() }

// value returns the page as the CEL response variable.
func (p *Page) value() map[string]any {
	headers := make(map[string]any, len(p.Headers))
	for k, vs := range p.Headers {
		list := make([]any, len(vs))
		for i, v := range vs {
			list[i] = v
		}
		headers[strings.ToLower(k)] = list
	}
	return map[string]any{"status": int64(p.Status), "headers": headers, "body": p.Body}
}

// Input is the data an extract is evaluated on.
type Input struct {
	Pages   []Page
	Vars    map[string]string
	Account map[string]any
	Target  map[string]any
	Now     time.Time
	// Labels are added to every sample, in increasing precedence (account,
	// target, job labels).
	Labels []map[string]string
}

// Limits bounds an evaluation (settings.mapping).
type Limits struct {
	MaxItems   int
	MaxSamples int
	// LabelConflict is "override" or "error".
	LabelConflict string
}

// Result is the outcome of an evaluation.
type Result struct {
	Samples []model.Sample
	Items   int
	// Inventory holds the inventory items, in item order, without duplicate
	// keys.
	Inventory []InventoryItem
	// MappingErrors counts the samples skipped because of an error.
	MappingErrors int
	// ErrorsByMetric counts them per metric name; filter and inventory
	// errors count under "filter" and "inventory".
	ErrorsByMetric map[string]int
	// Errors holds the first mapping errors, for logs.
	Errors []error
}

const maxReportedErrors = 10

func (r *Result) mappingError(metric string, err error) {
	r.MappingErrors++
	if r.ErrorsByMetric == nil {
		r.ErrorsByMetric = map[string]int{}
	}
	r.ErrorsByMetric[metric]++
	if len(r.Errors) < maxReportedErrors {
		r.Errors = append(r.Errors, err)
	}
}

// ErrLimitExceeded reports a run producing too many items or samples.
var ErrLimitExceeded = errors.New("mapping limit exceeded")

// Items returns the items of one page: extract.items, or the body when it
// is a list.
func (x *Extract) Items(page *Page, in *Input) ([]ref.Val, error) {
	act := &Activation{Response: page.value(), Vars: in.Vars, Account: in.Account, Target: in.Target, Now: in.Now}
	if x.items == nil {
		if _, ok := page.Body.([]any); !ok {
			return nil, errors.New("the response body is not a list: set extract.items")
		}
		return ToList(adapt(page.Body))
	}
	v, err := x.items.Eval(act)
	if err != nil {
		return nil, fmt.Errorf("extract.items: %w", err)
	}
	items, err := ToList(v)
	if err != nil {
		return nil, fmt.Errorf("extract.items: %w", err)
	}
	return items, nil
}

// Evaluate maps every item of every page to samples. Errors on one item or
// one metric skip the sample and are counted; errors in extract.items and
// exceeded limits fail the evaluation.
func (x *Extract) Evaluate(in *Input, lim Limits) (*Result, error) {
	res := &Result{}
	seen := map[string]bool{}
	for i := range in.Pages {
		page := &in.Pages[i]
		items, err := x.Items(page, in)
		if err != nil {
			return nil, err
		}
		res.Items += len(items)
		if lim.MaxItems > 0 && res.Items > lim.MaxItems {
			return nil, fmt.Errorf("%w: more than %d items", ErrLimitExceeded, lim.MaxItems)
		}
		act := &Activation{Response: page.value(), Vars: in.Vars, Account: in.Account, Target: in.Target, Now: in.Now}
		for _, item := range items {
			act.Item = item
			if x.filter != nil {
				keep, err := evalBool(x.filter, act)
				if err != nil {
					res.mappingError("filter", fmt.Errorf("extract.filter: %w", err))
					continue
				}
				if !keep {
					continue
				}
			}
			if x.key != nil {
				x.inventoryItem(res, act, seen)
			}
			for _, m := range x.metrics {
				x.emit(res, m, act, in, lim, page.ReceivedAt)
			}
			for _, m := range x.info {
				x.emit(res, m, act, in, lim, page.ReceivedAt)
			}
		}
	}
	if len(x.post) > 0 {
		responses := make([]any, len(in.Pages))
		var last time.Time
		for i := range in.Pages {
			responses[i] = in.Pages[i].value()
			last = in.Pages[i].ReceivedAt
		}
		act := &Activation{Responses: responses, Vars: in.Vars, Account: in.Account, Target: in.Target, Now: in.Now}
		for _, m := range x.post {
			x.emit(res, m, act, in, lim, last)
		}
	}
	if lim.MaxSamples > 0 && len(res.Samples) > lim.MaxSamples {
		return nil, fmt.Errorf("%w: more than %d samples", ErrLimitExceeded, lim.MaxSamples)
	}
	return res, nil
}

func (x *Extract) emit(res *Result, m *metric, act *Activation, in *Input, lim Limits, collected time.Time) {
	fail := func(err error) { res.mappingError(m.name, fmt.Errorf("%s (%s): %w", m.path, m.name, err)) }
	if m.skipIf != nil {
		skip, err := evalBool(m.skipIf, act)
		if err != nil {
			fail(fmt.Errorf("skipIf: %w", err))
			return
		}
		if skip {
			return
		}
	}
	s := model.Sample{Name: m.name, Type: m.typ, Help: m.help, Unit: m.unit, Value: 1, Timestamp: collected}
	if m.value != nil {
		v, err := m.value.Eval(act)
		if err == nil {
			s.Value, err = ToFloat(v)
		}
		if err != nil {
			fail(fmt.Errorf("value: %w", err))
			return
		}
	}
	if m.timestamp != nil {
		v, err := m.timestamp.Eval(act)
		var t time.Time
		var ok bool
		if err == nil {
			t, ok, err = ToTimestamp(v)
		}
		if err != nil {
			fail(fmt.Errorf("timestamp: %w", err))
			return
		}
		if ok {
			s.Timestamp, s.SourceTimestamp = t, true
		}
	}
	if m.accumulate {
		if s.Value < 0 {
			fail(fmt.Errorf("value: a window value to accumulate must not be negative, got %v", s.Value))
			return
		}
		s.Accumulate, s.Window = true, s.Timestamp.UTC().Format(time.RFC3339Nano)
		if m.window != nil {
			v, err := m.window.Eval(act)
			if err == nil {
				s.Window, err = windowString(v)
			}
			if err != nil {
				fail(fmt.Errorf("windowField: %w", err))
				return
			}
		}
	}
	labels := make(map[string]string, len(m.labels))
	for _, l := range m.labels {
		v, err := l.prg.Eval(act)
		var str string
		if err == nil {
			str, err = ToLabel(v)
		}
		if err != nil {
			fail(fmt.Errorf("label %s: %w", l.name, err))
			return
		}
		labels[l.name] = str
	}
	// Configured labels override the mapping (job > target > account >
	// mapping), unless conflicts are errors.
	mapped := make(map[string]string, len(labels))
	for k, v := range labels {
		mapped[k] = v
	}
	for _, extra := range in.Labels {
		for _, k := range sortedKeys(extra) {
			if v, ok := mapped[k]; ok && v != "" && v != extra[k] && lim.LabelConflict == "error" {
				fail(fmt.Errorf("label %s set by the mapping (%q) conflicts with a configured label (%q)", k, v, extra[k]))
				return
			}
			labels[k] = extra[k]
		}
	}
	s.Labels = model.LabelsFromMap(labels)
	res.Samples = append(res.Samples, s)
}

// inventoryItem evaluates the inventory key and fields of the current item.
func (x *Extract) inventoryItem(res *Result, act *Activation, seen map[string]bool) {
	v, err := x.key.Eval(act)
	var key string
	if err == nil {
		key, err = ToLabel(v)
	}
	if err == nil && key == "" {
		err = errors.New("empty key")
	}
	if err != nil {
		res.mappingError("inventory", fmt.Errorf("inventory key: %w", err))
		return
	}
	if seen[key] {
		return
	}
	item := InventoryItem{Key: key, Fields: map[string]any{}}
	for _, name := range sortedKeys(x.fields) {
		fv, err := x.fields[name].Eval(act)
		if err != nil {
			res.mappingError("inventory", fmt.Errorf("inventory field %s: %w", name, err))
			return
		}
		item.Fields[name] = fv.Value()
	}
	seen[key] = true
	res.Inventory = append(res.Inventory, item)
}

func evalBool(p *Program, act *Activation) (bool, error) {
	v, err := p.Eval(act)
	if err != nil {
		return false, err
	}
	return ToBool(v)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
