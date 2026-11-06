package main

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"maps"
	"slices"
	"strings"

	"github.com/goccy/go-yaml"

	"github.com/kentrow/tapyr/internal/buildinfo"
	"github.com/kentrow/tapyr/internal/config"
	"github.com/kentrow/tapyr/providers"
)

// categoriesFile is the controlled vocabulary of the catalog categories,
// embedded with the built-in packages (ADR 0011).
const categoriesFile = "categories.yaml"

// category is an entry of categoriesFile.
type category struct {
	Name        config.Name `json:"name" yaml:"name"`
	Description string      `json:"description" yaml:"description"`
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

// catalogSchemaVersion is the version of the output of tapyr provider list
// --output json, an interface of the site repository: it changes when a
// field is removed or changes meaning, not when one is added (see
// docs/spec/09-cli-api-ui.md, "Provider catalog output").
const catalogSchemaVersion = 1

type catalogJSON struct {
	SchemaVersion int               `json:"schemaVersion"`
	TapyrVersion  string            `json:"tapyrVersion"`
	Categories    []category        `json:"categories"`
	Providers     []catalogProvider `json:"providers"`
}

type catalogProvider struct {
	Name        config.Name        `json:"name"`
	Version     string             `json:"version"`
	DisplayName string             `json:"displayName,omitempty"`
	Description string             `json:"description,omitempty"`
	Docs        string             `json:"docs,omitempty"`
	Categories  []config.Name      `json:"categories,omitempty"`
	Maintainers []string           `json:"maintainers,omitempty"`
	Tier        string             `json:"tier,omitempty"`
	Validation  *config.Validation `json:"validation,omitempty"`
	Origin      string             `json:"origin"`
	Status      string             `json:"status"`
	Readme      string             `json:"readme,omitempty"`
	Simulator   bool               `json:"simulator"`
	Auth        string             `json:"auth"`
	Templates   []catalogTemplate  `json:"templates"`
	Limits      []catalogLimit     `json:"limits"`
}

type catalogTemplate struct {
	Name            config.Name `json:"name"`
	Description     string      `json:"description,omitempty"`
	Path            string      `json:"path,omitempty"`
	DefaultInterval string      `json:"defaultInterval"`
	Metrics         []string    `json:"metrics"`
}

type catalogLimit struct {
	Name        config.Name         `json:"name"`
	Kind        config.LimitKind    `json:"kind"`
	Value       string              `json:"value"`
	Scope       []string            `json:"scope"`
	Description string              `json:"description,omitempty"`
	Source      *config.LimitSource `json:"source,omitempty"`
}

// listedPackage is a package of tapyr provider list, in precedence order.
type listedPackage struct {
	pkg        *config.Package
	builtin    bool
	overridden bool
}

// printCatalogJSON writes the packages and the catalog categories as JSON.
func printCatalogJSON(e *env, listed []listedPackage) error {
	cats, err := catalogCategories()
	if err != nil {
		return err
	}
	out := catalogJSON{
		SchemaVersion: catalogSchemaVersion,
		TapyrVersion:  buildinfo.Get().Version,
		Categories:    cats,
		Providers:     []catalogProvider{},
	}
	for _, l := range listed {
		out.Providers = append(out.Providers, catalogEntry(l))
	}
	enc := json.NewEncoder(e.stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

func catalogEntry(l listedPackage) catalogProvider {
	p := l.pkg.Provider
	m := p.Metadata
	c := catalogProvider{
		Name: m.Name, Version: m.Version, DisplayName: m.DisplayName, Description: m.Description, Docs: m.Docs,
		Categories: m.Categories, Maintainers: m.Maintainers, Tier: m.Tier, Validation: m.Validation,
		Origin: l.pkg.Origin, Status: "active", Simulator: l.pkg.Simulation != nil, Auth: "none",
		Templates: []catalogTemplate{}, Limits: []catalogLimit{},
	}
	if l.builtin {
		c.Origin = "builtin"
		if _, err := fs.Stat(l.pkg.FS, "README.md"); err == nil {
			c.Readme = "providers/" + string(m.Name) + "/README.md"
		}
	}
	if l.overridden {
		c.Status = "overridden"
	}
	if a := p.Spec.Auth; a != nil && a.Type != "" {
		c.Auth = string(a.Type)
		if a.Type == config.AuthPlugin && a.Plugin != nil {
			c.Auth += ":" + a.Plugin.Name
		}
	}
	templates := slices.Clone(l.pkg.JobTemplates)
	slices.SortFunc(templates, func(a, b *config.JobTemplate) int {
		return strings.Compare(string(a.Metadata.Name), string(b.Metadata.Name))
	})
	for _, t := range templates {
		ct := catalogTemplate{
			Name: t.Metadata.Name, Description: t.Metadata.Description,
			DefaultInterval: shortDuration(t.Spec.DefaultInterval.Std()), Metrics: templateMetrics(t),
		}
		if t.Spec.Request != nil {
			ct.Path = t.Spec.Request.Path
		}
		c.Templates = append(c.Templates, ct)
	}
	for _, lim := range p.Spec.Limits {
		lim = config.LimitWithDefaults(lim)
		cl := catalogLimit{Name: lim.Name, Kind: lim.Kind, Value: limitValue(lim), Scope: []string{}, Description: lim.Description, Source: lim.Source}
		if lim.Scope != nil {
			cl.Scope = append(cl.Scope, lim.Scope.By...)
		}
		c.Limits = append(c.Limits, cl)
	}
	return c
}

// templateMetrics returns the sorted metric names of a template.
func templateMetrics(t *config.JobTemplate) []string {
	names := map[string]bool{}
	if e := t.Spec.Extract; e != nil {
		for _, m := range e.Metrics {
			names[m.Name] = true
		}
		for _, m := range e.Post {
			names[m.Name] = true
		}
		for _, m := range e.Info {
			names[m.Name] = true
		}
	}
	return slices.Sorted(maps.Keys(names))
}

// limitValue describes the value of a limit for humans: "10/s, burst 20",
// "10 concurrent requests", "100000 per month, reset on day 1 at 00:00 UTC".
func limitValue(l config.Limit) string {
	switch l.Kind {
	case config.LimitRate:
		if l.Rate == nil {
			return ""
		}
		return fmt.Sprintf("%s, burst %d", l.Rate, l.Burst)
	case config.LimitConcurrency:
		if l.Max == nil {
			return ""
		}
		return fmt.Sprintf("%d concurrent requests", l.Max.Value)
	case config.LimitQuota:
		if l.Max == nil || l.Window == nil {
			return ""
		}
		value := fmt.Sprint(l.Max.Value)
		if l.Max.Auto {
			value = fmt.Sprintf("auto (%d until learned)", l.InitialMax)
		}
		w := l.Window
		switch w.Period {
		case "":
			return fmt.Sprintf("%s per rolling %s", value, shortDuration(w.Rolling.Std()))
		case "minute", "hour":
			return fmt.Sprintf("%s per %s", value, w.Period)
		case "day":
			return fmt.Sprintf("%s per day, reset at %s %s", value, w.ResetAt, w.Timezone)
		case "week":
			return fmt.Sprintf("%s per week, reset on %s at %s %s", value, w.WeekStart, w.ResetAt, w.Timezone)
		case "month":
			return fmt.Sprintf("%s per month, reset on day %d at %s %s", value, w.MonthDay, w.ResetAt, w.Timezone)
		}
	}
	return ""
}
