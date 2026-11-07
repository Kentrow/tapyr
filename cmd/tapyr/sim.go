package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/spf13/cobra"

	"github.com/kentrow/tapyr/internal/auth/plugins"
	"github.com/kentrow/tapyr/internal/clock"
	"github.com/kentrow/tapyr/internal/collector"
	"github.com/kentrow/tapyr/internal/config"
	"github.com/kentrow/tapyr/internal/provider"
	"github.com/kentrow/tapyr/internal/sim"
)

func newSimCmd(e *env) *cobra.Command {
	var name, listen, listenBase, scenario string
	var all bool
	var providerPaths []string
	var seed uint64
	var timeScale float64
	cmd := &cobra.Command{
		Use:   "sim",
		Short: "Run provider simulators",
		Long: "Serve the simulator of a provider package: its fixtures, with the auth,\n" +
			"pagination and limits declared by the provider (429 with Retry-After and\n" +
			"quota headers). --scenario overlays faults; --time-scale accelerates the\n" +
			"simulated clock. --all serves every simulator from --listen-base, at the\n" +
			"portOffset of its package (the next free offsets, in provider name order,\n" +
			"for packages without one).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if all == (name != "") {
				return &exitError{code: exitFailure, err: errors.New("give either --provider or --all")}
			}
			loader, builtin, err := newLoader()
			if err != nil {
				return err
			}
			var pkgs []*config.Package
			for _, dir := range providerPaths {
				found, err := provider.FromPath(loader, dir)
				if err != nil {
					return &exitError{code: exitInvalidConfig, err: err}
				}
				pkgs = append(pkgs, found...)
			}
			pkgs = append(pkgs, builtin...)
			clk := e.clock
			if timeScale != 1 {
				if timeScale <= 0 {
					return &exitError{code: exitFailure, err: fmt.Errorf("invalid --time-scale %v (want a positive factor)", timeScale)}
				}
				clk = clock.NewScaled(e.clock, timeScale, e.clock.Now())
			}
			if !all {
				pkg := provider.Find(pkgs, name)
				if pkg == nil {
					return &exitError{code: exitFailure, err: fmt.Errorf("unknown provider %q (providers with a simulator: %s)", name, strings.Join(simulated(pkgs), ", "))}
				}
				sc, err := scenarioOf(pkg, scenario, true)
				if err != nil {
					return &exitError{code: exitFailure, err: err}
				}
				return serveSims(cmd.Context(), e, []simTarget{{pkg: pkg, listen: listen, scenario: sc}}, clk, seed, timeScale)
			}
			host, port, err := net.SplitHostPort(listenBase)
			first, perr := strconv.Atoi(port)
			if err != nil || perr != nil || first <= 0 {
				return &exitError{code: exitFailure, err: fmt.Errorf("invalid --listen-base %q (want host:port)", listenBase)}
			}
			names := simulated(pkgs)
			offsets, err := portOffsets(pkgs, names)
			if err != nil {
				return &exitError{code: exitInvalidConfig, err: err}
			}
			var targets []simTarget
			found := false
			for _, n := range names {
				pkg := provider.Find(pkgs, n)
				sc, err := scenarioOf(pkg, scenario, false)
				if err != nil {
					return &exitError{code: exitFailure, err: err}
				}
				found = found || sc != nil
				targets = append(targets, simTarget{pkg: pkg, listen: net.JoinHostPort(host, strconv.Itoa(first+offsets[n])), scenario: sc})
			}
			if scenario != "" && scenario != "default" && !found {
				return &exitError{code: exitFailure, err: fmt.Errorf("no simulator has a %q scenario", scenario)}
			}
			return serveSims(cmd.Context(), e, targets, clk, seed, timeScale)
		},
	}
	cmd.Flags().StringVar(&name, "provider", "", "provider package to simulate")
	cmd.Flags().BoolVar(&all, "all", false, "serve every simulator, at its port offset from --listen-base")
	cmd.Flags().StringVar(&listen, "listen", "127.0.0.1:8081", "listen address")
	cmd.Flags().StringVar(&listenBase, "listen-base", "127.0.0.1:8081", "address of the first simulator with --all")
	cmd.Flags().StringArrayVar(&providerPaths, "provider-path", nil, "directory of user provider packages, repeatable")
	cmd.Flags().StringVar(&scenario, "scenario", "", "scenario of the package (sim/scenarios), for example quota-tight; with --all, applied to the simulators that have it")
	cmd.Flags().Uint64Var(&seed, "seed", 42, "seed of the simulated randomness")
	cmd.Flags().Float64Var(&timeScale, "time-scale", 1, "simulated time runs this many times faster than real time")
	return cmd
}

type simTarget struct {
	pkg      *config.Package
	listen   string
	scenario *config.Scenario
}

// simulated lists the packages with a simulator, by name, a user package
// hiding the built-in package of the same name.
func simulated(pkgs []*config.Package) []string {
	var names []string
	for _, p := range pkgs {
		n := string(p.Provider.Metadata.Name)
		if p.Simulation != nil && !slices.Contains(names, n) && provider.Find(pkgs, n) == p {
			names = append(names, n)
		}
	}
	slices.Sort(names)
	return names
}

// portOffsets returns the port offset of every simulator of tapyr sim --all:
// the portOffset of its package, or for the packages without one, the
// offsets after the largest declared one, in name order. Two packages may
// not declare the same offset.
func portOffsets(pkgs []*config.Package, names []string) (map[string]int, error) {
	offsets := map[string]int{}
	owner := map[int]string{}
	next := 0
	for _, n := range names {
		o := provider.Find(pkgs, n).Simulation.Spec.PortOffset
		if o == nil {
			continue
		}
		if other, taken := owner[*o]; taken {
			return nil, fmt.Errorf("simulators %s and %s both declare portOffset %d", other, n, *o)
		}
		owner[*o], offsets[n] = n, *o
		next = max(next, *o+1)
	}
	for _, n := range names {
		if _, ok := offsets[n]; !ok {
			offsets[n] = next
			next++
		}
	}
	return offsets, nil
}

// scenarioOf returns the scenario of a package, nil for the default
// behavior. A package without the scenario is an error when required.
func scenarioOf(pkg *config.Package, name string, required bool) (*config.Scenario, error) {
	if name == "" {
		return nil, nil
	}
	if sc := pkg.Scenarios[name]; sc != nil {
		return sc, nil
	}
	if name == "default" || !required {
		return nil, nil
	}
	names := make([]string, 0, len(pkg.Scenarios))
	for n := range pkg.Scenarios {
		names = append(names, n)
	}
	slices.Sort(names)
	return nil, fmt.Errorf("unknown scenario %q for provider %s (available: %s)", name, pkg.Provider.Metadata.Name, strings.Join(names, ", "))
}

// serveSims serves simulators until ctx is done or one of them fails.
func serveSims(ctx context.Context, e *env, targets []simTarget, clk clock.Clock, seed uint64, timeScale float64) error {
	type server struct {
		target simTarget
		ln     net.Listener
		s      *sim.Server
	}
	servers := make([]server, 0, len(targets))
	defer func() {
		for _, s := range servers {
			_ = s.ln.Close()
		}
	}()
	for _, t := range targets {
		s, err := sim.New(t.pkg, sim.Options{Logger: e.logger, Clock: clk, Scenario: t.scenario, Seed: seed, Plugins: plugins.Builtin()})
		if err != nil {
			return &exitError{code: exitInvalidConfig, err: err}
		}
		ln, err := e.listen(ctx, "tcp", t.listen)
		if err != nil {
			return &exitError{code: exitFailure, err: err}
		}
		servers = append(servers, server{target: t, ln: ln, s: s})
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errs := make(chan error, len(servers))
	var wg sync.WaitGroup
	for _, s := range servers {
		sc := ""
		if s.target.scenario != nil {
			sc = string(s.target.scenario.Metadata.Name)
		}
		e.logger.Info("simulator listening", "component", "sim", "provider", string(s.target.pkg.Provider.Metadata.Name),
			"address", s.ln.Addr().String(), "scenario", sc, "time_scale", timeScale)
		wg.Go(func() {
			if err := collector.Serve(ctx, s.ln, s.s.Handler(), "", ""); err != nil {
				errs <- err
				cancel()
			}
		})
	}
	wg.Wait()
	close(errs)
	return <-errs
}
