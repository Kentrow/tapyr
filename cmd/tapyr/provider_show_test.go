package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/kentrow/tapyr/internal/config"
)

// TestProviderShowExample checks the promise of tapyr provider show: for
// every built-in package, the example it prints, with the output it
// suggests uncommented, is a configuration that validates and collects.
func TestProviderShowExample(t *testing.T) {
	for _, name := range builtinNames(t) {
		t.Run(name, func(t *testing.T) {
			res := execute(t, nil, "provider", "show", name, "--example")
			if res.code != exitOK || res.stderr != "" {
				t.Fatalf("exit %d\n%s", res.code, res.stderr)
			}
			// The suggested output comes last; an example can hold commented
			// documents of its own.
			i := strings.LastIndex(res.stdout, "# ---\n")
			example, suggested := res.stdout[:max(i, 0)], res.stdout[max(i, 0)+len("# ---\n"):]
			if i < 0 {
				t.Fatalf("no output suggested after the example:\n%s", res.stdout)
			}
			var output strings.Builder
			for line := range strings.Lines(suggested) {
				output.WriteString(strings.TrimPrefix(line, "# "))
			}
			dir := t.TempDir()
			config := example + "---\n" + output.String()
			if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(config), 0o600); err != nil {
				t.Fatal(err)
			}
			res = execute(t, nil, "validate", "--config", dir)
			if res.code != exitOK || res.stderr != "" {
				t.Errorf("the example with its output does not validate (exit %d):\n%s%s\n%s", res.code, res.stderr, res.stdout, config)
			}
		})
	}
}

// TestProviderShow checks the description of every built-in package: it
// names every secret, param, template and limit, and keeps to 80 columns
// but for links. The text for meraki-dashboard is kept in
// testdata/provider-show.txt (go test ./cmd/tapyr -run TestProviderShow
// -update).
func TestProviderShow(t *testing.T) {
	var catalog catalogJSON
	res := execute(t, nil, "provider", "list", "--output", "json")
	if err := json.Unmarshal([]byte(res.stdout), &catalog); err != nil {
		t.Fatal(err)
	}
	for _, p := range catalog.Providers {
		t.Run(string(p.Name), func(t *testing.T) {
			res := execute(t, nil, "provider", "show", string(p.Name))
			if res.code != exitOK || res.stderr != "" {
				t.Fatalf("exit %d\n%s", res.code, res.stderr)
			}
			var names []string
			for _, s := range p.Secrets {
				names = append(names, "  "+string(s.Name)+" ")
			}
			for _, s := range p.Params {
				names = append(names, "  "+string(s.Name)+" ")
			}
			for _, tmpl := range p.Templates {
				names = append(names, "  "+string(tmpl.Name)+"\n")
			}
			for _, l := range p.Limits {
				names = append(names, "  "+string(l.Name)+"\n")
			}
			for _, n := range names {
				if !strings.Contains(res.stdout, n) {
					t.Errorf("no %q in:\n%s", n, res.stdout)
				}
			}
			for line := range strings.Lines(res.stdout) {
				if len(strings.TrimRight(line, "\n")) > showWidth && !strings.Contains(line, "://") {
					t.Errorf("line over %d columns: %q", showWidth, line)
				}
			}
			if p.Name != "meraki-dashboard" {
				return
			}
			path := filepath.Join("testdata", "provider-show.txt")
			if *update {
				if err := os.WriteFile(path, []byte(res.stdout), 0o600); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if diff := diffLines(string(want), res.stdout); diff != "" {
				t.Errorf("the output differs from %s (go test ./cmd/tapyr -run TestProviderShow -update):\n%s", path, diff)
			}
		})
	}
}

func TestProviderShowJSON(t *testing.T) {
	var catalog catalogJSON
	res := execute(t, nil, "provider", "list", "--output", "json")
	if err := json.Unmarshal([]byte(res.stdout), &catalog); err != nil {
		t.Fatal(err)
	}
	res = execute(t, nil, "provider", "show", "juniper-mist", "--output", "json")
	if res.code != exitOK {
		t.Fatalf("exit %d\n%s", res.code, res.stderr)
	}
	var got catalogProvider
	if err := json.Unmarshal([]byte(res.stdout), &got); err != nil {
		t.Fatal(err)
	}
	for _, want := range catalog.Providers {
		if want.Name == "juniper-mist" {
			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("show --output json differs from the catalog entry (-list +show):\n%s", diff)
			}
			return
		}
	}
	t.Fatal("juniper-mist is not in the catalog")
}

func TestProviderShowErrors(t *testing.T) {
	tests := []struct {
		args []string
		want string
	}{
		{[]string{"meraki"}, `unknown provider "meraki" (did you mean "meraki-dashboard"?)`},
		{[]string{"nothing-close"}, `unknown provider "nothing-close"; tapyr provider list shows the packages`},
		{[]string{"meraki-dashboard", "--output", "yaml"}, `invalid --output "yaml" (want text or json)`},
	}
	for _, tt := range tests {
		res := execute(t, nil, append([]string{"provider", "show"}, tt.args...)...)
		if res.code != exitFailure || !strings.Contains(res.stderr, tt.want) {
			t.Errorf("%v: exit %d, stderr %q, want %q", tt.args, res.code, res.stderr, tt.want)
		}
	}
}

// TestProviderShowProviderPath checks that a package of --provider-path
// overrides the built-in one of the same name.
func TestProviderShowProviderPath(t *testing.T) {
	dir := t.TempDir()
	if err := os.CopyFS(filepath.Join(dir, "example-sim"), os.DirFS(filepath.Join("..", "..", "providers", "example-sim"))); err != nil {
		t.Fatal(err)
	}
	res := execute(t, nil, "provider", "show", "example-sim", "--provider-path", dir)
	if res.code != exitOK || !strings.Contains(res.stdout, filepath.Join(dir, "example-sim")) {
		t.Errorf("exit %d, output:\n%s%s", res.code, res.stderr, res.stdout)
	}
	res = execute(t, nil, "provider", "list", "--provider-path", dir)
	if res.code != exitOK || !strings.Contains(res.stdout, "overridden") {
		t.Errorf("provider list --provider-path: exit %d, output:\n%s%s", res.code, res.stderr, res.stdout)
	}
}

func TestLimitScopeWords(t *testing.T) {
	tests := []struct {
		by   []string
		want string
	}{
		{nil, "for the whole Tapyr process"},
		{[]string{"instance"}, "for the whole Tapyr process"},
		{[]string{"provider"}, "shared by every account"},
		{[]string{"account"}, "per account"},
		{[]string{"var:organizationId"}, "per organizationId"},
		{[]string{"account", "endpoint"}, "per account and endpoint"},
		{[]string{"credential"}, "per credential (accounts with the same secrets share it)"},
	}
	for _, tt := range tests {
		if got := limitScopeWords(tt.by); got != tt.want {
			t.Errorf("limitScopeWords(%v) = %q, want %q", tt.by, got, tt.want)
		}
	}
}

func TestDiscoveryOrder(t *testing.T) {
	templates := []catalogTemplate{
		{Name: "a-devices", ForEach: "c-sites"},
		{Name: "b-orgs"},
		{Name: "c-sites", ForEach: "b-orgs"},
		{Name: "d-health"},
	}
	var got []config.Name
	for _, tmpl := range discoveryOrder(templates) {
		got = append(got, tmpl.Name)
	}
	if want := []config.Name{"b-orgs", "d-health", "c-sites", "a-devices"}; !cmp.Equal(got, want) {
		t.Errorf("discoveryOrder = %v, want %v", got, want)
	}
}

func TestValidationWords(t *testing.T) {
	tests := []struct {
		v    *config.Validation
		want string
	}{
		{nil, "unknown"},
		{&config.Validation{Status: "simulator"}, "on the simulator only"},
		{&config.Validation{Status: "real-platform", Date: "2026-11-07", PlatformVersion: "v1"}, "on a real platform (v1, 2026-11-07) and the simulator"},
	}
	for _, tt := range tests {
		if got := validationWords(tt.v); got != tt.want {
			t.Errorf("validationWords(%+v) = %q, want %q", tt.v, got, tt.want)
		}
	}
}

// builtinNames returns the names of the built-in packages.
func builtinNames(t *testing.T) []string {
	t.Helper()
	var catalog catalogJSON
	res := execute(t, nil, "provider", "list", "--output", "json")
	if err := json.Unmarshal([]byte(res.stdout), &catalog); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range catalog.Providers {
		names = append(names, string(p.Name))
	}
	return names
}
