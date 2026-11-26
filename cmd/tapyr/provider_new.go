package main

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kentrow/tapyr/internal/config"
	"github.com/kentrow/tapyr/internal/provider"
)

// skeleton holds the files of tapyr provider new, rendered by replacing the
// placeholders of skeletonReplacer (not text/template: the files hold Go
// templates and Grafana variables of their own).
//
//go:embed all:skeleton
var skeleton embed.FS

// maxSkeletonName is the longest package name whose template name
// (<name>-devices) is still a valid name.
const maxSkeletonName = 63 - len("-devices")

func newProviderNewCmd(e *env) *cobra.Command {
	var parent string
	cmd := &cobra.Command{
		Use:   "new <name>",
		Short: "Write the skeleton of a provider package",
		Long: "Write a minimal provider package in <dir>/<name>: provider.yaml with a\n" +
			"rate limit and a daily quota, one job template, a simulator with the\n" +
			"default, quota-tight and bad-day scenarios, a contract test and its\n" +
			"expected samples, an example, a dashboard and a README. It passes\n" +
			"tapyr provider lint --catalog as written; the TODO comments mark what to\n" +
			"replace with what the vendor documentation says.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if err := config.Name(name).Validate(); err != nil {
				return &exitError{code: exitInvalidConfig, err: fmt.Errorf("provider name %q: %w", name, err)}
			}
			if len(name) > maxSkeletonName {
				return &exitError{code: exitInvalidConfig, err: fmt.Errorf("provider name %q: at most %d characters, so that the template name %s-devices fits in 63", name, maxSkeletonName, name)}
			}
			dir := filepath.Join(parent, name)
			if err := newPackage(cmd.Context(), dir, name, nil); err != nil {
				return &exitError{code: exitInvalidConfig, err: err}
			}
			fmt.Fprintf(e.stdout, "Wrote the provider package %s in %s.\n\n", name, dir)
			fmt.Fprintf(e.stdout, "Next steps (docs/providers.md, \"Writing a package\"):\n")
			fmt.Fprintf(e.stdout, "  1. Replace the TODO comments with what the vendor documentation says.\n")
			fmt.Fprintf(e.stdout, "  2. Serve the simulator: tapyr sim --provider-path %s --provider %s --listen 127.0.0.1:8081\n", parent, name)
			fmt.Fprintf(e.stdout, "  3. Lint after every change: tapyr provider lint %s (--update rewrites tests/expected.prom)\n", dir)
			fmt.Fprintf(e.stdout, "  4. Use it: providerPaths: [%s] in a configuration, or --provider-path %s\n", parent, parent)
			return nil
		},
	}
	cmd.Flags().StringVar(&parent, "dir", ".", "parent directory of the package, created when missing")
	return cmd
}

// newPackage writes the skeleton of the package name in dir, generates its
// expected samples and lints it; edit, for tests, changes the files before
// the lint. dir must not exist, and is removed again when anything fails.
func newPackage(ctx context.Context, dir, name string, edit func(dir string) error) (err error) {
	if _, err := os.Stat(dir); err == nil {
		return fmt.Errorf("%s already exists", dir)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o750); err != nil {
		return err
	}
	if err := os.Mkdir(dir, 0o750); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(dir)
		}
	}()
	if err := writeSkeleton(dir, name); err != nil {
		return err
	}
	if edit != nil {
		if err := edit(dir); err != nil {
			return err
		}
	}
	return lintSkeleton(ctx, dir)
}

// writeSkeleton renders the skeleton files for the package name into dir.
func writeSkeleton(dir, name string) error {
	r := skeletonReplacer(name)
	return fs.WalkDir(skeleton, "skeleton", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := skeleton.ReadFile(p)
		if err != nil {
			return err
		}
		rel := r.Replace(strings.TrimSuffix(strings.TrimPrefix(p, "skeleton/"), ".tmpl"))
		target := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return err
		}
		return os.WriteFile(target, []byte(r.Replace(string(data))), 0o600)
	})
}

// skeletonReplacer replaces the placeholders of the skeleton files: the
// package name, its metric prefix, the prefix of its environment variables,
// its display name and the uid of its dashboard.
func skeletonReplacer(name string) *strings.Replacer {
	prefix := strings.ReplaceAll(name, "-", "_")
	words := strings.Split(name, "-")
	for i, w := range words {
		words[i] = strings.ToUpper(w[:1]) + w[1:]
	}
	uid := "tapyr-" + name
	if len(uid) > 40 { // the longest uid Grafana accepts
		uid = uid[:40]
	}
	return strings.NewReplacer(
		"__NAME__", name,
		"__PREFIX__", prefix,
		"__ENV__", strings.ToUpper(prefix),
		"__DISPLAY__", strings.Join(words, " "),
		"__UID__", uid,
	)
}

// lintSkeleton writes the expected samples of the package in dir, then
// lints it as tapyr provider lint --catalog does: a skeleton that does not
// pass is a bug of the skeleton.
func lintSkeleton(ctx context.Context, dir string) error {
	loader, builtin, err := newLoader()
	if err != nil {
		return err
	}
	cats, err := catalogCategories()
	if err != nil {
		return err
	}
	for _, update := range []bool{true, false} {
		pkgs, err := provider.FromPath(loader, dir)
		if err != nil {
			return err
		}
		if len(pkgs) != 1 {
			return fmt.Errorf("%s: want one provider package, found %d", dir, len(pkgs))
		}
		problems := lintPackage(ctx, loader, pkgs[0], builtin, dir, update)
		if !update {
			problems = append(problems, lintCatalog(pkgs[0], cats)...)
		}
		if len(problems) > 0 {
			return fmt.Errorf("the skeleton does not pass the lint (a bug of tapyr provider new):\n  %s", strings.Join(problems, "\n  "))
		}
	}
	return nil
}
