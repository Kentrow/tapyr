package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/kentrow/tapyr/internal/config"
)

func TestCatalogCategories(t *testing.T) {
	cats, err := catalogCategories()
	if err != nil {
		t.Fatal(err)
	}
	if len(cats) == 0 {
		t.Fatal("no category")
	}
	name := regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
	seen := map[string]bool{}
	for _, c := range cats {
		if !name.MatchString(string(c.Name)) || c.Description == "" {
			t.Errorf("category %+v: a name is a DNS label and a description is required", c)
		}
		if seen[string(c.Name)] {
			t.Errorf("category %s is listed twice", c.Name)
		}
		seen[string(c.Name)] = true
	}
}

// Markers of the provider tables generated from the catalog metadata of the
// built-in packages.
const (
	tableBegin = "<!-- begin provider table: generated from the provider packages by TestProviderTables (go test ./cmd/tapyr -run TestProviderTables -update), do not edit -->\n"
	tableEnd   = "<!-- end provider table -->\n"
)

// TestProviderTables keeps the provider tables of the README and of
// docs/providers.md in agreement with the packages. Fix them with:
// go test ./cmd/tapyr -run TestProviderTables -update.
func TestProviderTables(t *testing.T) {
	_, builtin, err := newLoader()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join("..", "..")
	for _, f := range []struct{ path, prefix string }{
		{"README.md", "providers/"},
		{filepath.Join("docs", "providers.md"), "../providers/"},
	} {
		path := filepath.Join(root, f.path)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		before, rest, ok := strings.Cut(string(data), tableBegin)
		_, after, ok2 := strings.Cut(rest, tableEnd)
		if !ok || !ok2 {
			t.Fatalf("%s has no provider table markers", f.path)
		}
		got := before + tableBegin + providerTable(builtin, f.prefix) + tableEnd + after
		if *update {
			if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if got != string(data) {
			t.Errorf("the provider table of %s is out of date: go test ./cmd/tapyr -run TestProviderTables -update", f.path)
		}
	}
}

// providerTable renders the table of the packages, with links to their
// README under prefix.
func providerTable(pkgs []*config.Package, prefix string) string {
	var b strings.Builder
	b.WriteString("\n| Package | API | Tier | Validation |\n|---|---|---|---|\n")
	for _, p := range pkgs {
		m := p.Provider.Metadata
		fmt.Fprintf(&b, "| [`%s`](%s%s/README.md) | %s | %s | %s |\n", m.Name, prefix, m.Name, m.Description, tierText(m.Tier), validationText(m.Validation))
	}
	b.WriteString("\n")
	return b.String()
}

func tierText(tier string) string {
	switch tier {
	case "official":
		return "Official"
	case "community":
		return "Community"
	}
	return "Unknown"
}

func validationText(v *config.Validation) string {
	switch {
	case v == nil:
		return "Unknown"
	case v.Status == "real-platform":
		return fmt.Sprintf("Real platform (%s, %s) and simulator", v.PlatformVersion, v.Date)
	}
	return "Simulator only"
}

// TestProviderListJSON keeps the catalog output of the built-in packages, an
// interface of the site repository, visible in pull requests. Fix it with:
// go test ./cmd/tapyr -run TestProviderListJSON -update.
func TestProviderListJSON(t *testing.T) {
	res := execute(t, nil, "provider", "list", "--output", "json")
	if res.code != 0 {
		t.Fatalf("exit %d\n%s", res.code, res.stderr)
	}
	var out catalogJSON
	if err := json.Unmarshal([]byte(res.stdout), &out); err != nil {
		t.Fatal(err)
	}
	if out.SchemaVersion != catalogSchemaVersion || out.TapyrVersion == "" {
		t.Errorf("schemaVersion %d, tapyrVersion %q", out.SchemaVersion, out.TapyrVersion)
	}
	// The version of the binary is not part of the golden file.
	got := strings.Replace(res.stdout, fmt.Sprintf("%q", out.TapyrVersion), `"0.0.0-test"`, 1)
	path := filepath.Join("testdata", "provider-list.json")
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if diff := diffLines(string(want), got); diff != "" {
		t.Errorf("the output differs from %s (go test ./cmd/tapyr -run TestProviderListJSON -update):\n%s", path, diff)
	}
}

func TestProviderListOverride(t *testing.T) {
	dir := t.TempDir()
	if err := os.CopyFS(filepath.Join(dir, "providers", "example-sim"), os.DirFS(filepath.Join("..", "..", "providers", "example-sim"))); err != nil {
		t.Fatal(err)
	}
	config := fmt.Sprintf("apiVersion: tapyr/v1alpha1\nkind: Settings\nmetadata: { name: local }\nspec:\n  providerPaths: [%q]\n", filepath.Join(dir, "providers"))
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	res := execute(t, nil, "provider", "list", "--output", "json", "--config", filepath.Join(dir, "config.yaml"))
	if res.code != 0 {
		t.Fatalf("exit %d\n%s", res.code, res.stderr)
	}
	var out catalogJSON
	if err := json.Unmarshal([]byte(res.stdout), &out); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, p := range out.Providers {
		if p.Name == "example-sim" {
			got = append(got, fmt.Sprintf("%s %s readme=%q", p.Status, filepath.Base(p.Origin), p.Readme))
		}
	}
	want := []string{`active example-sim readme=""`, `overridden builtin readme="providers/example-sim/README.md"`}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("example-sim entries:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestLimitValue(t *testing.T) {
	rate := &config.Rate{Count: 10, Per: time.Second}
	tests := []struct {
		name  string
		limit config.Limit
		want  string
	}{
		{"rate with the default burst", config.Limit{Kind: config.LimitRate, Rate: rate}, "10/s, burst 10"},
		{"rate", config.Limit{Kind: config.LimitRate, Rate: &config.Rate{Count: 600, Per: time.Minute}, Burst: 20}, "600/m, burst 20"},
		{"concurrency", config.Limit{Kind: config.LimitConcurrency, Max: &config.IntOrAuto{Value: 4}}, "4 concurrent requests"},
		{"quota per hour", quota(1000, config.Window{Period: "hour"}), "1000 per hour"},
		{"quota per day", quota(5000, config.Window{Period: "day"}), "5000 per day, reset at 00:00 UTC"},
		{"quota per day in a timezone", quota(5000, config.Window{Period: "day", ResetAt: "07:00", Timezone: "Europe/Paris"}), "5000 per day, reset at 07:00 Europe/Paris"},
		{"quota per week", quota(9, config.Window{Period: "week"}), "9 per week, reset on monday at 00:00 UTC"},
		{"quota per month", quota(100000, config.Window{Period: "month", MonthDay: 15}), "100000 per month, reset on day 15 at 00:00 UTC"},
		{"rolling quota", quota(10000, config.Window{Rolling: config.Duration(24 * time.Hour)}), "10000 per rolling 24h"},
		{
			"learned quota",
			config.Limit{Kind: config.LimitQuota, Max: &config.IntOrAuto{Auto: true}, InitialMax: 5000, Window: &config.Window{Period: "day"}},
			"auto (5000 until learned) per day, reset at 00:00 UTC",
		},
		{"quota without window", config.Limit{Kind: config.LimitQuota, Max: &config.IntOrAuto{Value: 1}}, ""},
		{"rate without rate", config.Limit{Kind: config.LimitRate}, ""},
		{"concurrency without max", config.Limit{Kind: config.LimitConcurrency}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := limitValue(config.LimitWithDefaults(tt.limit)); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func quota(maximum int64, w config.Window) config.Limit {
	return config.Limit{Kind: config.LimitQuota, Max: &config.IntOrAuto{Value: maximum}, Window: &w}
}
