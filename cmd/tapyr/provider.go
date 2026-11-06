package main

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/kentrow/tapyr/internal/config"
	"github.com/kentrow/tapyr/internal/provider"
)

func newProviderListCmd(e *env, flags *globalFlags) *cobra.Command {
	var format string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List built-in and user provider packages",
		Long: "List the built-in provider packages and, when a configuration is given,\n" +
			"the packages of its providerPaths, in precedence order. --output json\n" +
			"adds the catalog metadata, the templates and the limits of every package,\n" +
			"and the catalog categories (the input of the provider catalog).",
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			if format != "table" && format != "json" {
				return &exitError{code: exitFailure, err: fmt.Errorf("invalid --output %q (want table or json)", format)}
			}
			loader, builtin, err := newLoader()
			if err != nil {
				return err
			}
			var pkgs []*config.Package
			if paths := configPaths(e, flags); len(paths) > 0 {
				if pkgs, err = userPackages(loader, paths); err != nil {
					return err
				}
			}
			listed := make([]listedPackage, 0, len(pkgs)+len(builtin))
			seen := map[config.Name]bool{}
			for i, p := range append(pkgs, builtin...) {
				name := p.Provider.Metadata.Name
				listed = append(listed, listedPackage{pkg: p, builtin: i >= len(pkgs), overridden: seen[name]})
				seen[name] = true
			}
			if format == "json" {
				return printCatalogJSON(e, listed)
			}
			w := tabwriter.NewWriter(e.stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tVERSION\tTEMPLATES\tSIMULATOR\tSTATUS\tORIGIN")
			for _, l := range listed {
				status := "active"
				if l.overridden {
					status = "overridden"
				}
				sim := "no"
				if l.pkg.Simulation != nil {
					sim = "yes"
				}
				fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\t%s\n", l.pkg.Provider.Metadata.Name, l.pkg.Provider.Metadata.Version,
					len(l.pkg.JobTemplates), sim, status, l.pkg.Origin)
			}
			return w.Flush()
		},
	}
	cmd.Flags().StringVar(&format, "output", "table", "output format: table or json")
	return cmd
}

// userPackages loads the packages of the providerPaths of a configuration.
func userPackages(loader *config.Loader, paths []string) ([]*config.Package, error) {
	cfg, err := loader.Parse(paths)
	if err != nil {
		return nil, &exitError{code: exitInvalidConfig, err: err}
	}
	if cfg.Settings == nil {
		return nil, nil
	}
	var pkgs []*config.Package
	for _, dir := range cfg.Settings.Spec.ProviderPaths {
		found, err := provider.FromPath(loader, dir)
		if err != nil {
			return nil, &exitError{code: exitInvalidConfig, err: err}
		}
		pkgs = append(pkgs, found...)
	}
	return pkgs, nil
}
