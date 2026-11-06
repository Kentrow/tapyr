package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

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
