package series

import (
	"slices"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kentrow/tapyr/internal/config"
	"github.com/kentrow/tapyr/internal/model"
	"github.com/kentrow/tapyr/internal/processing"
	"github.com/kentrow/tapyr/internal/state"
)

var t0 = time.Date(2026, time.November, 2, 12, 0, 0, 0, time.UTC)

func sample(name, serial string, ts time.Time, source bool) model.Sample {
	return model.Sample{
		Name: name, Type: model.Gauge, Labels: model.LabelsFromMap(map[string]string{"serial": serial}),
		Value: 1, Timestamp: ts, SourceTimestamp: source,
	}
}

func keys(samples []model.Sample) []string {
	var out []string
	for i := range samples {
		out = append(out, samples[i].Labels.Get("serial")+"@"+samples[i].Timestamp.Format("15:04"))
	}
	return out
}

func serials(series []model.Series) []string {
	var out []string
	for _, s := range series {
		out = append(out, s.Name+"/"+s.Labels.Get("serial"))
	}
	return out
}

var outputs = []Output{{Name: "prom"}, {Name: "remote", Dedup: true}}

func TestSuccessStalenessAndDedup(t *testing.T) {
	r := New()
	// First run: everything is new.
	d := r.Success("devices", []model.Sample{
		sample("up", "A", t0, true), sample("up", "B", t0, true), sample("clients", "A", t0.Add(time.Minute), false),
	}, outputs, t0)
	if got := keys(d["remote"].Samples); len(got) != 3 || d["remote"].Deduplicated != 0 || len(d["remote"].Stale) != 0 {
		t.Fatalf("first run = %+v", d["remote"])
	}

	// Second run: A has no newer source timestamp (dropped for push, kept
	// for pull), B is gone, C appears; samples stamped with the collection
	// time are never deduplicated.
	d = r.Success("devices", []model.Sample{
		sample("up", "A", t0, true), sample("up", "C", t0, true), sample("clients", "A", t0.Add(time.Minute), false),
	}, outputs, t0.Add(5*time.Minute))
	if diff := cmp.Diff([]string{"C@12:00", "A@12:01"}, keys(d["remote"].Samples)); diff != "" {
		t.Errorf("remote samples (-want +got):\n%s", diff)
	}
	if d["remote"].Deduplicated != 1 || len(d["prom"].Samples) != 3 || d["prom"].Deduplicated != 0 {
		t.Errorf("dedup: remote %d, prom %d samples", d["remote"].Deduplicated, len(d["prom"].Samples))
	}
	for _, o := range []string{"prom", "remote"} {
		if diff := cmp.Diff([]string{"up/B"}, serials(d[o].Stale)); diff != "" {
			t.Errorf("%s stale (-want +got):\n%s", o, diff)
		}
	}

	// A newer source timestamp goes through; B comes back as a new series
	// (its timestamps were forgotten when it went stale).
	d = r.Success("devices", []model.Sample{
		sample("up", "A", t0.Add(time.Hour), true), sample("up", "B", t0, true),
	}, outputs, t0.Add(10*time.Minute))
	if diff := cmp.Diff([]string{"A@13:00", "B@12:00"}, keys(d["remote"].Samples)); diff != "" {
		t.Errorf("remote samples (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{"clients/A", "up/C"}, serials(d["remote"].Stale)); diff != "" {
		t.Errorf("stale (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{"up/A", "up/B"}, serials(r.Series("devices"))); diff != "" {
		t.Errorf("series (-want +got):\n%s", diff)
	}
}

func TestExpireRemoveAndSnapshot(t *testing.T) {
	r := New()
	r.Success("devices", []model.Sample{sample("up", "A", t0, true)}, outputs, t0)
	r.Success("sites", []model.Sample{sample("site", "S", t0, false)}, outputs, t0)
	r.Success("gone", []model.Sample{sample("x", "G", t0, false)}, outputs, t0)

	staleAfter := func(name string) (time.Duration, bool) {
		switch name {
		case "devices":
			return 15 * time.Minute, true
		case "sites":
			return 3 * time.Hour, true
		}
		return 0, false
	}
	if diff := cmp.Diff([]string{"gone"}, r.Expired(t0.Add(15*time.Minute), staleAfter)); diff != "" {
		t.Errorf("expired at 15m (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{"devices", "gone"}, r.Expired(t0.Add(16*time.Minute), staleAfter)); diff != "" {
		t.Errorf("expired at 16m (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{"up/A"}, serials(r.Expire("devices").Series)); diff != "" {
		t.Errorf("expire (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{"x/G"}, serials(r.Remove("gone").Series)); diff != "" {
		t.Errorf("remove (-want +got):\n%s", diff)
	}
	if r.Remove("gone").Series != nil || r.Expire("gone").Series != nil {
		t.Error("removing twice returned series")
	}
	// An expired instance is not expired again until it has series.
	if diff := cmp.Diff([]string(nil), r.Expired(t0.Add(time.Hour), staleAfter)); diff != "" {
		t.Errorf("expired at 1h (-want +got):\n%s", diff)
	}

	changed, removed := r.Snapshot()
	if len(changed) != 2 || changed[0].Instance != "devices" || len(changed[0].Series) != 0 || changed[1].Instance != "sites" {
		t.Errorf("snapshot changed = %+v", changed)
	}
	if diff := cmp.Diff([]string{"gone"}, removed); diff != "" {
		t.Errorf("snapshot removed (-want +got):\n%s", diff)
	}
	if changed, removed := r.Snapshot(); len(changed)+len(removed) != 0 {
		t.Errorf("second snapshot = %v %v", changed, removed)
	}
	// The dedup timestamps of an expired instance survive: a recovering run
	// with the same source timestamp is deduplicated.
	d := r.Success("devices", []model.Sample{sample("up", "A", t0, true)}, outputs, t0.Add(time.Hour))
	if d["remote"].Deduplicated != 1 || len(d["remote"].Stale) != 0 {
		t.Errorf("recovering run = %+v", d["remote"])
	}

	// Restore rebuilds the registry from snapshots.
	changed, _ = r.Snapshot()
	r2 := New()
	if skipped := r2.Restore(append(changed, changed[0])); skipped != 0 {
		t.Errorf("skipped %d", skipped)
	}
	d = r2.Success("devices", []model.Sample{sample("up", "A", t0, true)}, outputs, t0.Add(2*time.Hour))
	if d["remote"].Deduplicated != 1 {
		t.Errorf("dedup after restore = %+v", d["remote"])
	}
	d = r2.Success("devices", nil, outputs, t0.Add(3*time.Hour))
	if diff := cmp.Diff([]string{"up/A"}, serials(d["prom"].Stale)); diff != "" {
		t.Errorf("stale after restore (-want +got):\n%s", diff)
	}
	changed[0].Series = []string{"invalid"}
	if skipped := New().Restore(changed); skipped != 1 {
		t.Errorf("skipped %d invalid keys, want 1", skipped)
	}
}

// site is a sample of a device of a site: the rules of the tests drop the
// serial and keep the site.
func site(name, serial, site string, ts time.Time, source bool) model.Sample {
	s := sample(name, serial, ts, source)
	s.Labels = model.LabelsFromMap(map[string]string{"serial": serial, "site": site})
	return s
}

func sites(series []model.Series) []string {
	var out []string
	for _, s := range series {
		out = append(out, s.Name+"/"+s.Labels.Get("serial")+"/"+s.Labels.Get("site"))
	}
	return out
}

func mustProcessor(t *testing.T, rules ...config.ProcessingRule) *processing.Processor {
	t.Helper()
	p, err := processing.Compile(rules)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestProcessedOutput checks staleness and deduplication on the series an
// output with processing rules receives (ADR 0012).
func TestProcessedOutput(t *testing.T) {
	bySite := mustProcessor(t,
		config.ProcessingRule{Action: config.ProcessingLabelDrop, Regex: "serial"},
		config.ProcessingRule{Action: config.ProcessingDrop, SourceLabels: []string{"__name__"}, Regex: "down"},
	)
	outs := []Output{{Name: "prom"}, {Name: "hosted", Dedup: true, Processor: bySite}}
	r := New()
	d := r.Success("devices", []model.Sample{
		site("up", "A", "north", t0, true), site("up", "B", "south", t0, true), site("down", "A", "north", t0, false),
	}, outs, t0)
	if h := d["hosted"]; len(h.Samples) != 2 || h.Filtered != 1 || h.Duplicates != 0 || len(h.Stale) != 0 ||
		h.Samples[0].Labels.Get("serial") != "" {
		t.Fatalf("first run = %+v", h)
	}

	// B is gone: its processed series goes stale; the raw one for prom. A
	// is deduplicated on its processed key, C merges with A (same site,
	// same timestamp): a duplicate.
	d = r.Success("devices", []model.Sample{
		site("up", "A", "north", t0, true), site("up", "C", "north", t0, true), site("down", "A", "north", t0, false),
	}, outs, t0.Add(5*time.Minute))
	h := d["hosted"]
	if diff := cmp.Diff([]string{"up//south"}, sites(h.Stale)); diff != "" {
		t.Errorf("hosted stale (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{"up/B/south"}, sites(d["prom"].Stale)); diff != "" {
		t.Errorf("prom stale (-want +got):\n%s", diff)
	}
	if len(h.Samples) != 0 || h.Deduplicated != 1 || h.Duplicates != 1 || h.Filtered != 1 {
		t.Errorf("second run = %+v", h)
	}

	// The rules change (reload): the series sent with the old rules go
	// stale, the new ones are not deduplicated against the old keys.
	keepAll := mustProcessor(t, config.ProcessingRule{Action: config.ProcessingDrop, SourceLabels: []string{"__name__"}, Regex: "down"})
	outs[1].Processor = keepAll
	d = r.Success("devices", []model.Sample{site("up", "A", "north", t0.Add(time.Hour), true)}, outs, t0.Add(10*time.Minute))
	h = d["hosted"]
	if diff := cmp.Diff([]string{"up//north"}, sites(h.Stale)); diff != "" {
		t.Errorf("stale after a rule change (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{"A@13:00"}, keys(h.Samples)); diff != "" {
		t.Errorf("samples after a rule change (-want +got):\n%s", diff)
	}

	// The rules are removed: back to the raw series, the processed ones
	// that differ go stale once.
	outs[1].Processor = nil
	d = r.Success("devices", []model.Sample{site("up", "A", "north", t0.Add(2*time.Hour), true), site("down", "A", "north", t0, false)},
		outs, t0.Add(15*time.Minute))
	if h = d["hosted"]; len(h.Stale) != 0 || len(h.Samples) != 2 {
		t.Errorf("after removing the rules = %+v", h)
	}
	d = r.Success("devices", []model.Sample{site("up", "A", "north", t0.Add(3*time.Hour), true)}, outs, t0.Add(20*time.Minute))
	if diff := cmp.Diff([]string{"down/A/north"}, sites(d["hosted"].Stale)); diff != "" {
		t.Errorf("raw staleness again (-want +got):\n%s", diff)
	}
}

// TestProcessedWithdrawal checks what a withdrawn instance leaves to an
// output with processing rules, known or derived after a restart.
func TestProcessedWithdrawal(t *testing.T) {
	bySite := mustProcessor(t, config.ProcessingRule{Action: config.ProcessingLabelDrop, Regex: "serial"})
	outs := []Output{{Name: "prom"}, {Name: "hosted", Dedup: true, Processor: bySite}}
	r := New()
	r.Success("devices", []model.Sample{site("up", "A", "north", t0, false), site("up", "B", "north", t0, false)}, outs, t0)
	w := r.Remove("devices")
	if diff := cmp.Diff([]string{"up//north"}, sites(w.For("hosted", bySite))); diff != "" {
		t.Errorf("hosted withdrawal (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{"up/A/north", "up/B/north"}, sites(w.For("prom", nil))); diff != "" {
		t.Errorf("prom withdrawal (-want +got):\n%s", diff)
	}

	// After a restart, the processed series are derived from the snapshot
	// with the current rules.
	r = New()
	r.Success("devices", []model.Sample{site("up", "A", "north", t0, false)}, outs[:1], t0)
	snaps, _ := r.Snapshot()
	restored := New()
	restored.Restore(snaps)
	if diff := cmp.Diff([]string{"up//north"}, sites(restored.Expire("devices").For("hosted", bySite))); diff != "" {
		t.Errorf("derived withdrawal (-want +got):\n%s", diff)
	}
	restored.Restore(snaps)
	d := restored.Success("devices", []model.Sample{site("up", "A", "south", t0.Add(time.Minute), false)}, outs, t0.Add(time.Minute))
	if diff := cmp.Diff([]string{"up//north"}, sites(d["hosted"].Stale)); diff != "" {
		t.Errorf("derived staleness after a restart (-want +got):\n%s", diff)
	}
}

// up returns one up sample per serial at ts.
func up(ts time.Time, serials ...string) []model.Sample {
	var out []model.Sample
	for _, s := range serials {
		out = append(out, sample("up", s, ts, false))
	}
	return out
}

func TestSeriesLimit(t *testing.T) {
	r := New()
	hosted := Output{Name: "hosted", MaxSeries: 4}
	local := Output{Name: "local"}
	both := []Output{hosted, local}
	all := func(string) bool { return true }
	check := func(t *testing.T, d Delivery, sent []string, refused, refusedSeries int) {
		t.Helper()
		var got []string
		for i := range d.Samples {
			got = append(got, d.Samples[i].Labels.Get("serial"))
		}
		if !slices.Equal(got, sent) || d.Refused != refused || d.RefusedSeries != refusedSeries {
			t.Errorf("sent %v (refused %d samples, %d series), want %v (%d, %d)", got, d.Refused, d.RefusedSeries, sent, refused, refusedSeries)
		}
	}

	// Two instances share the limit of 4: a gets 3, b the last place.
	check(t, r.Success("a", up(t0, "a1", "a2", "a3"), both, t0)["hosted"], []string{"a1", "a2", "a3"}, 0, 0)
	d := r.Success("b", up(t0, "b1", "b2", "b3"), both, t0)
	check(t, d["hosted"], []string{"b1"}, 2, 2)
	check(t, d["local"], []string{"b1", "b2", "b3"}, 0, 0) // no limit on the other output
	if s, refused := r.OutputSeries(hosted, all); s != 4 || refused != 2 {
		t.Errorf("hosted: %d series, %d refused, want 4 and 2", s, refused)
	}
	if s, refused := r.OutputSeries(local, all); s != 6 || refused != 0 {
		t.Errorf("local: %d series, %d refused, want 6 and 0", s, refused)
	}

	// The series already sent keep flowing; a new one of a waits.
	check(t, r.Success("a", up(t0.Add(time.Minute), "a0", "a1", "a2", "a3"), both, t0)["hosted"], []string{"a1", "a2", "a3"}, 1, 1)
	// A series that stops frees its place: b takes it, in key order.
	check(t, r.Success("a", up(t0.Add(2*time.Minute), "a1", "a2"), both, t0)["hosted"], []string{"a1", "a2"}, 0, 0)
	check(t, r.Success("b", up(t0.Add(2*time.Minute), "b1", "b2", "b3"), both, t0)["hosted"], []string{"b1", "b2"}, 1, 1)
	// A removed instance frees all its places.
	r.Remove("a")
	check(t, r.Success("b", up(t0.Add(3*time.Minute), "b1", "b2", "b3"), both, t0)["hosted"], []string{"b1", "b2", "b3"}, 0, 0)

	// A limit lowered below the series sent keeps them and admits none.
	lower := Output{Name: "hosted", MaxSeries: 1}
	check(t, r.Success("b", up(t0.Add(4*time.Minute), "b1", "b2", "b3", "b4"), []Output{lower}, t0)["hosted"], []string{"b1", "b2", "b3"}, 1, 1)
}

func TestSeriesLimitAfterRestart(t *testing.T) {
	r := New()
	r.Restore([]state.SeriesSnapshot{
		{Instance: "a", LastSuccess: t0, Series: seriesKeys(up(t0, "a1", "a2", "a3"))},
		{Instance: "b", LastSuccess: t0, Series: seriesKeys(up(t0, "b1", "b2"))},
		{Instance: "c", LastSuccess: t0, Series: seriesKeys(up(t0, "c1"))},
	})
	hosted := Output{Name: "hosted", MaxSeries: 4, Processor: mustProcessor(t, config.ProcessingRule{
		Action: config.ProcessingDrop, SourceLabels: []string{"serial"}, Regex: "a3",
	})}
	routed := func(instance string) bool { return instance != "c" }
	// Before any run: the series each instance would send, after the rules.
	if s, _ := r.OutputSeries(hosted, routed); s != 4 {
		t.Errorf("before reconcile: %d series, want 4 (a1, a2, b1, b2)", s)
	}
	// Reconcile admits up to the limit, in instance and key order.
	r.Reconcile(Output{Name: hosted.Name, MaxSeries: 3, Processor: hosted.Processor}, routed)
	if s, _ := r.OutputSeries(hosted, routed); s != 3 {
		t.Errorf("after reconcile: %d series, want 3", s)
	}
	d := r.Success("b", up(t0.Add(time.Minute), "b1", "b2"), []Output{{Name: hosted.Name, MaxSeries: 3, Processor: hosted.Processor}}, t0)
	if got := d["hosted"]; len(got.Samples) != 1 || got.Samples[0].Labels.Get("serial") != "b1" || got.RefusedSeries != 1 {
		t.Errorf("b after the restart: %+v", got)
	}
}

func seriesKeys(samples []model.Sample) []string {
	var out []string
	for i := range samples {
		out = append(out, samples[i].SeriesKey())
	}
	return out
}
