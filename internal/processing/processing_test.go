package processing

import (
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kentrow/tapyr/internal/config"
	"github.com/kentrow/tapyr/internal/model"
)

var at = time.Date(2026, time.November, 10, 12, 0, 0, 0, time.UTC)

func sample(name string, labels map[string]string) model.Sample {
	return model.Sample{Name: name, Type: model.Gauge, Labels: model.LabelsFromMap(labels), Value: 1, Timestamp: at}
}

func ptr(s string) *string { return &s }

// render writes samples as name{labels}, in order.
func render(samples []model.Sample) string {
	var out []string
	for _, s := range samples {
		var ls []string
		for _, l := range s.Labels {
			ls = append(ls, l.Name+"="+l.Value)
		}
		out = append(out, s.Name+"{"+strings.Join(ls, ",")+"}")
	}
	return strings.Join(out, " ")
}

func TestSamples(t *testing.T) {
	in := []model.Sample{
		sample("mist_device_up", map[string]string{"mac": "02:00", "site": "north"}),
		sample("mist_device_clients", map[string]string{"mac": "02:00", "site": "north"}),
		sample("mist_site_clients", map[string]string{"site": "south"}),
	}
	tests := []struct {
		name         string
		rules        []config.ProcessingRule
		want         string
		wantFiltered int
	}{
		{name: "no rules", want: "mist_device_up{mac=02:00,site=north} mist_device_clients{mac=02:00,site=north} mist_site_clients{site=south}"},
		{
			name:  "drop by name",
			rules: []config.ProcessingRule{{Action: config.ProcessingDrop, SourceLabels: []string{"__name__"}, Regex: "mist_device_.*"}},
			want:  "mist_site_clients{site=south}", wantFiltered: 2,
		},
		{
			name:  "keep by name, anchored",
			rules: []config.ProcessingRule{{Action: config.ProcessingKeep, SourceLabels: []string{"__name__"}, Regex: "mist_device_up|clients"}},
			want:  "mist_device_up{mac=02:00,site=north}", wantFiltered: 2,
		},
		{
			name: "joined labels with the default separator",
			rules: []config.ProcessingRule{{
				Action: config.ProcessingDrop, SourceLabels: []string{"__name__", "site"}, Regex: "mist_device_up;north",
			}},
			want: "mist_device_clients{mac=02:00,site=north} mist_site_clients{site=south}", wantFiltered: 1,
		},
		{
			name: "custom separator, missing label is empty",
			rules: []config.ProcessingRule{{
				Action: config.ProcessingKeep, SourceLabels: []string{"site", "mac"}, Separator: ptr("/"), Regex: "south/",
			}},
			want: "mist_site_clients{site=south}", wantFiltered: 2,
		},
		{
			name:  "keep without regex keeps everything",
			rules: []config.ProcessingRule{{Action: config.ProcessingKeep, SourceLabels: []string{"site"}}},
			want:  "mist_device_up{mac=02:00,site=north} mist_device_clients{mac=02:00,site=north} mist_site_clients{site=south}",
		},
		{
			name:  "labeldrop",
			rules: []config.ProcessingRule{{Action: config.ProcessingLabelDrop, Regex: "si.*"}},
			want:  "mist_device_up{mac=02:00} mist_device_clients{mac=02:00} mist_site_clients{}",
		},
		{
			name:  "labelkeep",
			rules: []config.ProcessingRule{{Action: config.ProcessingLabelKeep, Regex: "__name__|site"}},
			want:  "mist_device_up{site=north} mist_device_clients{site=north} mist_site_clients{site=south}",
		},
		{
			name: "rules in order: a label dropped first no longer matches",
			rules: []config.ProcessingRule{
				{Action: config.ProcessingLabelDrop, Regex: "site"},
				{Action: config.ProcessingDrop, SourceLabels: []string{"site"}, Regex: "north"},
			},
			want: "mist_device_up{mac=02:00} mist_device_clients{mac=02:00} mist_site_clients{}",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := Compile(tt.rules)
			if err != nil {
				t.Fatal(err)
			}
			before := render(in)
			out, filtered, dups := p.Samples(in)
			if got := render(out); got != tt.want || filtered != tt.wantFiltered || dups != 0 {
				t.Errorf("got %s (%d filtered, %d duplicates), want %s (%d filtered)", got, filtered, dups, tt.want, tt.wantFiltered)
			}
			if render(in) != before {
				t.Error("the input samples were modified")
			}
		})
	}
}

func TestDuplicates(t *testing.T) {
	p, err := Compile([]config.ProcessingRule{{Action: config.ProcessingLabelDrop, Regex: "mac"}})
	if err != nil {
		t.Fatal(err)
	}
	later := sample("up", map[string]string{"mac": "02:02", "site": "north"})
	later.Timestamp = at.Add(time.Hour)
	in := []model.Sample{
		sample("up", map[string]string{"mac": "02:00", "site": "north"}),
		sample("up", map[string]string{"mac": "02:01", "site": "north"}),
		later, // same series, another timestamp: not a duplicate
	}
	out, filtered, dups := p.Samples(in)
	if len(out) != 2 || filtered != 0 || dups != 1 || !out[1].Timestamp.Equal(at.Add(time.Hour)) {
		t.Errorf("got %s, %d filtered, %d duplicates", render(out), filtered, dups)
	}
}

func TestSeries(t *testing.T) {
	in := []model.Series{
		{Name: "up", Labels: model.LabelsFromMap(map[string]string{"mac": "02:00", "site": "north"})},
		{Name: "up", Labels: model.LabelsFromMap(map[string]string{"mac": "02:01", "site": "north"})},
		{Name: "down", Labels: model.LabelsFromMap(map[string]string{"site": "north"})},
	}
	p, err := Compile([]config.ProcessingRule{
		{Action: config.ProcessingLabelDrop, Regex: "mac"},
		{Action: config.ProcessingDrop, SourceLabels: []string{"__name__"}, Regex: "down"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := p.Series(in)
	if keys := slices.Sorted(maps.Keys(got)); len(keys) != 1 || got[keys[0]].Name != "up" || got[keys[0]].Labels.Get("mac") != "" {
		t.Errorf("Series() = %v", got)
	}
	var none *Processor
	if len(none.Series(in)) != 3 {
		t.Error("a nil processor changed the series")
	}
	if out, _, _ := none.Samples([]model.Sample{sample("up", nil)}); len(out) != 1 {
		t.Error("a nil processor changed the samples")
	}
	if p, err := Compile(nil); p != nil || err != nil {
		t.Errorf("Compile(nil) = %v, %v", p, err)
	}
	if _, err := Compile([]config.ProcessingRule{{Action: config.ProcessingDrop, SourceLabels: []string{"x"}, Regex: "("}}); err == nil {
		t.Error("an invalid regex compiled")
	}
}

func TestMayKeep(t *testing.T) {
	name := []string{"__name__"}
	for _, tt := range []struct {
		name  string
		rules []config.ProcessingRule
		want  map[string]bool // metric -> may keep
	}{
		{"no rules", nil, map[string]bool{"mist_device_up": true}},
		{
			"drop by name",
			[]config.ProcessingRule{{Action: config.ProcessingDrop, SourceLabels: name, Regex: "mist_device_.*"}},
			map[string]bool{"mist_device_up": false, "mist_site_clients": true},
		},
		{
			"keep by name",
			[]config.ProcessingRule{{Action: config.ProcessingKeep, SourceLabels: name, Regex: "mist_site_.*"}},
			map[string]bool{"mist_device_up": false, "mist_site_clients": true},
		},
		{
			"labelkeep and labeldrop never drop a sample",
			[]config.ProcessingRule{
				{Action: config.ProcessingLabelDrop, Regex: "site"},
				{Action: config.ProcessingLabelKeep, Regex: "__name__|mac"},
			},
			map[string]bool{"mist_device_up": true},
		},
		{
			// A rule on a label value cannot be decided: the metric may be
			// kept, even if a later rule on the name would drop it.
			"a rule on a label value stops the evaluation",
			[]config.ProcessingRule{
				{Action: config.ProcessingDrop, SourceLabels: []string{"site"}, Regex: "north"},
				{Action: config.ProcessingDrop, SourceLabels: name, Regex: ".*"},
			},
			map[string]bool{"mist_device_up": true},
		},
		{
			"the name with another label is not decided",
			[]config.ProcessingRule{{Action: config.ProcessingDrop, SourceLabels: []string{"__name__", "site"}, Regex: "mist_device_up;.*"}},
			map[string]bool{"mist_device_up": true},
		},
		{
			"a drop by name before a rule on a label value is decided",
			[]config.ProcessingRule{
				{Action: config.ProcessingDrop, SourceLabels: name, Regex: "mist_device_.*"},
				{Action: config.ProcessingKeep, SourceLabels: []string{"site"}, Regex: "north"},
			},
			map[string]bool{"mist_device_up": false, "mist_site_clients": true},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p, err := Compile(tt.rules)
			if err != nil {
				t.Fatal(err)
			}
			for _, metric := range slices.Sorted(maps.Keys(tt.want)) {
				if got := p.MayKeep(metric); got != tt.want[metric] {
					t.Errorf("MayKeep(%s) = %t, want %t", metric, got, tt.want[metric])
				}
			}
		})
	}
}
