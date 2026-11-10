package main

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
)

// documentedSelfMetrics returns the self-metrics of the tables of
// 07-self-observability.md, with the series of the histograms.
func documentedSelfMetrics(t *testing.T) map[string]bool {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "docs", "spec", "07-self-observability.md"))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, m := range regexp.MustCompile("(?m)^\\| `(tapyr_[a-z0-9_]+)` \\| ([a-z]+) \\|").FindAllStringSubmatch(string(data), -1) {
		out[m[1]] = true
		if m[2] == "histogram" {
			for _, s := range []string{"_bucket", "_sum", "_count"} {
				out[m[1]+s] = true
			}
		}
	}
	if len(out) < 40 {
		t.Fatalf("only %d self-metrics found in the spec tables", len(out))
	}
	return out
}

// undocumented returns the tapyr_ identifiers of PromQL expressions that are
// not documented self-metrics.
func undocumented(exprs []string, metrics map[string]bool) []string {
	unknown := map[string]bool{}
	for _, e := range exprs {
		for _, id := range identifierPattern.FindAllString(stripStrings(e), -1) {
			if strings.HasPrefix(id, "tapyr_") && !metrics[id] {
				unknown[id] = true
			}
		}
	}
	return slices.Sorted(maps.Keys(unknown))
}

func TestOverviewDashboard(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "dashboards", "tapyr-overview.json"))
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		UID        string `json:"uid"`
		Title      string `json:"title"`
		Templating struct {
			List []struct {
				Type  string `json:"type"`
				Query any    `json:"query"`
			} `json:"list"`
		} `json:"templating"`
		Panels []dashboardPanel `json:"panels"`
	}
	if err := json.Unmarshal(data, &d); err != nil {
		t.Fatal(err)
	}
	if d.UID != "tapyr-overview" || d.Title == "" {
		t.Errorf("uid %q, title %q", d.UID, d.Title)
	}
	var exprs []string
	hasDatasource := false
	for _, v := range d.Templating.List {
		hasDatasource = hasDatasource || v.Type == "datasource"
		if q, ok := v.Query.(map[string]any); ok {
			exprs = append(exprs, q["query"].(string))
		}
	}
	if !hasDatasource {
		t.Error("no datasource variable")
	}
	var walk func([]dashboardPanel)
	walk = func(panels []dashboardPanel) {
		for _, p := range panels {
			for _, tg := range p.Targets {
				exprs = append(exprs, tg.Expr)
			}
			walk(p.Panels)
		}
	}
	walk(d.Panels)
	for _, p := range panelProblems("tapyr-overview.json", d.Panels) {
		t.Error(p)
	}
	if len(exprs) < 10 {
		t.Fatalf("only %d queries", len(exprs))
	}
	if u := undocumented(exprs, documentedSelfMetrics(t)); len(u) > 0 {
		t.Errorf("metrics that are not documented self-metrics: %v", u)
	}
}

func TestExampleAlerts(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "examples", "alerts", "tapyr-alerts.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Groups []struct {
			Rules []struct {
				Alert string `yaml:"alert"`
				Expr  string `yaml:"expr"`
			} `yaml:"rules"`
		} `yaml:"groups"`
	}
	if err := yaml.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	var exprs []string
	for _, g := range f.Groups {
		for _, r := range g.Rules {
			if r.Alert == "" || r.Expr == "" {
				t.Errorf("rule %q without an alert name or an expression", r.Alert)
			}
			exprs = append(exprs, r.Expr)
		}
	}
	if len(exprs) < 10 {
		t.Fatalf("only %d rules", len(exprs))
	}
	if u := undocumented(exprs, documentedSelfMetrics(t)); len(u) > 0 {
		t.Errorf("metrics that are not documented self-metrics: %v", u)
	}
	// Every alert has its section in docs/alerts.md.
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "alerts.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range f.Groups {
		for _, r := range g.Rules {
			if !strings.Contains(string(doc), "### "+r.Alert+"\n") {
				t.Errorf("docs/alerts.md has no section for %s", r.Alert)
			}
		}
	}
}
