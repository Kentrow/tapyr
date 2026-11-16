package main

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"regexp"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kentrow/tapyr/internal/buildinfo"
	"github.com/kentrow/tapyr/internal/config"
	"github.com/kentrow/tapyr/internal/problem"
)

// showWidth is the width the descriptions of tapyr provider show wrap at.
const showWidth = 80

func newProviderShowCmd(e *env, flags *globalFlags) *cobra.Command {
	var format string
	var example bool
	cmd := &cobra.Command{
		Use:   "show <name>",
		Short: "Describe a provider package",
		Long: "Describe a provider package: what it is and how it was validated, the\n" +
			"secrets and params an account gives it, its templates (interval, role,\n" +
			"metrics) and its limits with their sources. --example prints the example\n" +
			"configuration of the package, to copy and adapt; --output json its entry\n" +
			"of the provider catalog (see tapyr provider list --output json).",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if format != "text" && format != "json" {
				return &exitError{code: exitFailure, err: fmt.Errorf("invalid --output %q (want text or json)", format)}
			}
			listed, err := listPackages(e, flags)
			if err != nil {
				return err
			}
			l, err := findPackage(listed, args[0])
			if err != nil {
				return err
			}
			switch {
			case example:
				return printExample(e.stdout, l.pkg)
			case format == "json":
				enc := json.NewEncoder(e.stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(catalogEntry(l))
			}
			printProvider(e.stdout, catalogEntry(l))
			return nil
		},
	}
	cmd.Flags().StringVar(&format, "output", "text", "output format: text or json")
	cmd.Flags().BoolVar(&example, "example", false, "print the example configuration of the package")
	addProviderPathFlag(cmd, flags)
	return cmd
}

// findPackage returns the active package of a name, or an error naming the
// closest one.
func findPackage(listed []listedPackage, name string) (listedPackage, error) {
	var names []string
	for _, l := range listed {
		if !l.overridden && string(l.pkg.Provider.Metadata.Name) == name {
			return l, nil
		}
		names = append(names, string(l.pkg.Provider.Metadata.Name))
	}
	return listedPackage{}, &exitError{code: exitFailure, err: fmt.Errorf("unknown provider %q%s; tapyr provider list shows the packages",
		name, config.DidYouMean(name, names))}
}

// printExample writes the example configuration of a package
// (examples/*.yaml), the files one after the other, and how to add an
// output when it has none.
func printExample(w io.Writer, pkg *config.Package) error {
	files, err := fs.Glob(pkg.FS, "examples/*.yaml")
	if err != nil || len(files) == 0 {
		return &exitError{code: exitFailure, err: fmt.Errorf("the package %s has no example (examples/*.yaml)", pkg.Provider.Metadata.Name)}
	}
	hasOutput := false
	for i, f := range files {
		data, err := fs.ReadFile(pkg.FS, f)
		if err != nil {
			return fmt.Errorf("reading %s: %w", f, err)
		}
		if i > 0 {
			fmt.Fprintln(w, "---")
		}
		if len(files) > 1 {
			fmt.Fprintf(w, "# %s\n", f)
		}
		if _, err := w.Write(data); err != nil {
			return err
		}
		hasOutput = hasOutput || outputDocument.Match(data)
	}
	if !hasOutput {
		// Jobs that no output keeps are not collected (ADR 0013).
		fmt.Fprintf(w, `# This example declares no Output: without one, its jobs collect nothing.
# Add one, for example /metrics for Prometheus to scrape; see
# %s
# ---
# apiVersion: tapyr/v1alpha1
# kind: Output
# metadata: { name: prometheus }
# spec: { type: prometheus }
`, problem.DocsPage(buildinfo.Get().Version, "configure-accounts/#3-write-the-configuration"))
	}
	return nil
}

// outputDocument matches the kind line of an Output document.
var outputDocument = regexp.MustCompile(`(?m)^kind:\s*Output\s*$`)

// printProvider describes a package for humans.
func printProvider(w io.Writer, p catalogProvider) {
	title := string(p.Name) + " " + p.Version
	if p.DisplayName != "" {
		title += " - " + p.DisplayName
	}
	fmt.Fprintln(w, title)
	if p.Description != "" {
		fmt.Fprint(w, wrapWords("", strings.Fields(p.Description), showWidth))
	}
	fmt.Fprintln(w)
	field := func(key, value string) {
		fmt.Fprint(w, wrapWords(fmt.Sprintf("%-13s", key+":"), strings.Fields(value), showWidth))
	}
	field("Validation", validationWords(p.Validation))
	if p.Docs != "" {
		field("API docs", p.Docs)
	}
	origin := "built in"
	if p.Origin != "builtin" {
		origin = p.Origin
	}
	field("Origin", origin)
	sim := "none"
	if p.Simulator {
		sim = "tapyr sim --provider " + string(p.Name)
	}
	field("Simulator", sim)
	field("Auth", p.Auth)

	printParams(w, "Secrets, on the account as secrets: { NAME: { env: VAR } }:", p.Secrets)
	printParams(w, "Params, on the account as params: { NAME: VALUE }:", p.Params)

	fmt.Fprintf(w, "\nTemplates, named by a job as template: NAME (parents first):\n")
	for _, t := range discoveryOrder(p.Templates) {
		fmt.Fprintf(w, "  %s\n", t.Name)
		fmt.Fprint(w, wrapList("    ", templateWords(t), showWidth))
		if t.Description != "" {
			fmt.Fprint(w, wrapWords("    ", strings.Fields(t.Description), showWidth))
		}
		if len(t.Metrics) > 0 {
			fmt.Fprint(w, wrapList("    metrics: ", t.Metrics, showWidth))
		}
	}

	if len(p.Limits) > 0 {
		fmt.Fprintf(w, "\nLimits, which Tapyr keeps to:\n")
		for _, l := range p.Limits {
			fmt.Fprintf(w, "  %s\n", l.Name)
			fmt.Fprint(w, wrapList("    ", slices.DeleteFunc([]string{l.Value, limitScopeWords(l.Scope)}, func(s string) bool { return s == "" }), showWidth))
			if l.Description != "" {
				fmt.Fprint(w, wrapWords("    ", strings.Fields(l.Description), showWidth))
			}
			if s := limitSourceWords(l.Source); s != "" {
				fmt.Fprint(w, wrapWords("    source: ", strings.Fields(s), showWidth))
			}
		}
	}
	fmt.Fprintf(w, "\nExample configuration: tapyr provider show %s --example\n", p.Name)
}

func printParams(w io.Writer, title string, params []catalogParam) {
	if len(params) == 0 {
		return
	}
	fmt.Fprintf(w, "\n%s\n", title)
	width := 0
	for _, p := range params {
		width = max(width, len(p.Name))
	}
	for _, p := range params {
		need := "optional"
		if p.Required {
			need = "required"
		}
		prefix := fmt.Sprintf("  %-*s  %s  ", width, p.Name, need)
		desc := strings.Fields(p.Description)
		if len(desc) == 0 {
			fmt.Fprintln(w, strings.TrimRight(prefix, " "))
			continue
		}
		fmt.Fprint(w, wrapWords(prefix, desc, showWidth))
	}
}

// validationWords says how a package was validated.
func validationWords(v *config.Validation) string {
	switch {
	case v == nil:
		return "unknown"
	case v.Status == "real-platform":
		words := "on a real platform"
		if details := strings.Join(slices.DeleteFunc([]string{v.PlatformVersion, v.Date}, func(s string) bool { return s == "" }), ", "); details != "" {
			words += " (" + details + ")"
		}
		return words + " and the simulator"
	case v.Status == "simulator":
		return "on the simulator only"
	}
	return "unknown"
}

// discoveryOrder sorts templates parents first: by their depth in the
// discovery chain, then by name.
func discoveryOrder(templates []catalogTemplate) []catalogTemplate {
	parent := map[config.Name]config.Name{}
	for _, t := range templates {
		parent[t.Name] = t.ForEach
	}
	depth := func(name config.Name) int {
		d := 0
		for p := parent[name]; p != "" && d < len(templates); p = parent[p] {
			d++
		}
		return d
	}
	out := slices.Clone(templates)
	slices.SortStableFunc(out, func(a, b catalogTemplate) int { return depth(a.Name) - depth(b.Name) })
	return out
}

// templateWords says when a template runs and what it is for: "every 1h",
// "every 5m", "priority high", "for each item of meraki-organizations",
// "lists items for other templates".
func templateWords(t catalogTemplate) []string {
	words := []string{"every " + t.DefaultInterval}
	if t.DefaultPriority != "" && t.DefaultPriority != config.PriorityNormal {
		words = append(words, "priority "+string(t.DefaultPriority))
	}
	if t.ForEach != "" {
		words = append(words, "for each item of "+string(t.ForEach))
	}
	switch t.Role {
	case config.RoleInventory:
		words = append(words, "lists items for other templates")
	case config.RoleBoth:
		words = append(words, "also lists items for other templates")
	}
	return words
}

// limitScopeWords says what a limit is counted per: "per account", "per
// organizationId", "for the whole Tapyr process".
func limitScopeWords(by []string) string {
	if len(by) == 0 {
		by = []string{"instance"}
	}
	if len(by) == 1 {
		switch by[0] {
		case "instance":
			return "for the whole Tapyr process"
		case "provider":
			return "shared by every account"
		}
	}
	nouns := make([]string, 0, len(by))
	for _, d := range by {
		switch {
		case d == "instance":
			nouns = append(nouns, "Tapyr process")
		case d == "credential":
			nouns = append(nouns, "credential (accounts with the same secrets share it)")
		case strings.HasPrefix(d, "var:"):
			nouns = append(nouns, strings.TrimPrefix(d, "var:"))
		default:
			nouns = append(nouns, d)
		}
	}
	return "per " + strings.Join(nouns, " and ")
}

// limitSourceWords says where the value of a limit comes from.
func limitSourceWords(s *config.LimitSource) string {
	if s == nil {
		return ""
	}
	words := map[string]string{"vendor-docs": "vendor documentation", "measured": "measured", "assumed": "assumed, not documented"}[s.Kind]
	if words == "" {
		words = s.Kind
	}
	if s.Date != "" {
		words += " (" + s.Date + ")"
	}
	if s.URL != "" {
		words += ", " + s.URL
	}
	return words
}
