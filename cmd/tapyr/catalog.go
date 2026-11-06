package main

import (
	"fmt"
	"io/fs"

	"github.com/goccy/go-yaml"

	"github.com/kentrow/tapyr/internal/config"
	"github.com/kentrow/tapyr/providers"
)

// categoriesFile is the controlled vocabulary of the catalog categories,
// embedded with the built-in packages (ADR 0011).
const categoriesFile = "categories.yaml"

// category is an entry of categoriesFile.
type category struct {
	Name        config.Name `yaml:"name"`
	Description string      `yaml:"description"`
}

// catalogCategories reads the categories of the catalog.
func catalogCategories() ([]category, error) {
	data, err := fs.ReadFile(providers.FS, categoriesFile)
	if err != nil {
		return nil, fmt.Errorf("reading the catalog categories: %w", err)
	}
	var cats []category
	if err := yaml.UnmarshalWithOptions(data, &cats, yaml.Strict()); err != nil {
		return nil, fmt.Errorf("parsing the catalog categories: %w", err)
	}
	return cats, nil
}

// lintCatalog returns the problems of the catalog metadata of a package:
// tapyr provider lint --catalog requires the fields the schema leaves
// optional (see docs/spec/03-configuration.md, "Provider packages").
func lintCatalog(pkg *config.Package, cats []category) []string {
	known := map[config.Name]bool{}
	for _, c := range cats {
		known[c.Name] = true
	}
	meta := pkg.Provider.Metadata
	var problems []string
	add := func(format string, args ...any) {
		problems = append(problems, "catalog: "+fmt.Sprintf(format, args...))
	}
	if meta.DisplayName == "" {
		add("no metadata.displayName")
	}
	if len(meta.Categories) == 0 {
		add("no metadata.categories")
	}
	for _, c := range meta.Categories {
		if !known[c] {
			add("unknown category %q (see providers/%s)", c, categoriesFile)
		}
	}
	if len(meta.Maintainers) == 0 {
		add("no metadata.maintainers")
	}
	if meta.Tier == "" {
		add("no metadata.tier")
	}
	switch v := meta.Validation; {
	case v == nil:
		add("no metadata.validation")
	case v.Status == "real-platform" && (v.Date == "" || v.PlatformVersion == ""):
		add("metadata.validation needs a date and a platformVersion with status real-platform")
	}
	if v := meta.Validation; v != nil && v.Date != "" {
		if _, err := config.ParseDate(v.Date); err != nil {
			add("metadata.validation.date: %v", err)
		}
	}
	for _, l := range pkg.Provider.Spec.Limits {
		s := l.Source
		switch {
		case s == nil:
			add("limit %s has no source", l.Name)
			continue
		case s.Kind == "vendor-docs" && s.URL == "":
			add("limit %s: a vendor-docs source needs a url", l.Name)
		case s.Kind == "measured" && s.Date == "":
			add("limit %s: a measured source needs a date", l.Name)
		}
		if s.Date != "" {
			if _, err := config.ParseDate(s.Date); err != nil {
				add("limit %s: source.date: %v", l.Name, err)
			}
		}
	}
	return problems
}
