// Package collector is the process of tapyr run: it schedules the jobs of a
// compiled configuration through the budget engine, delivers their samples
// to the outputs, reconciles discovery into job instances, reloads the
// configuration, and serves the self-metrics, the health endpoints, the
// status API and the web UI (see docs/spec/02-architecture.md). cmd/tapyr
// wires its dependencies.
package collector

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kentrow/tapyr/internal/api"
	"github.com/kentrow/tapyr/internal/auth"
	"github.com/kentrow/tapyr/internal/budget"
	"github.com/kentrow/tapyr/internal/buildinfo"
	"github.com/kentrow/tapyr/internal/clock"
	"github.com/kentrow/tapyr/internal/config"
	"github.com/kentrow/tapyr/internal/model"
	"github.com/kentrow/tapyr/internal/output/prometheus"
	"github.com/kentrow/tapyr/internal/runtime"
	"github.com/kentrow/tapyr/internal/secret"
	"github.com/kentrow/tapyr/internal/selfobs"
	"github.com/kentrow/tapyr/internal/series"
	"github.com/kentrow/tapyr/internal/state"
	internalui "github.com/kentrow/tapyr/internal/ui"
)

// runsShutdownTimeout bounds the wait for runs in flight on shutdown and
// on reload.
const runsShutdownTimeout = 30 * time.Second

// Options are the dependencies of a Collector, wired by cmd/tapyr.
type Options struct {
	Clock clock.Clock
	// Listen opens the listener of the HTTP server.
	Listen func(ctx context.Context, network, address string) (net.Listener, error)
	Store  state.Store
	Logger *slog.Logger
	// Redactor removes the secrets and the tokens from the logs and from the
	// texts that leave the process; the tokens are added as they come.
	Redactor *selfobs.Redactor
	Secrets  *config.SecretSource
	// Salt is the salt of the credential keys, InstanceID the persisted
	// identifier of this instance, StateKey the key of the tokens kept in
	// the state store (TAPYR_STATE_KEY, empty for none).
	Salt       []byte
	InstanceID string
	StateKey   string
	// Plugins is the registry of the auth plugins.
	Plugins auth.Plugins
	// Load loads the configuration again on reload; Compile compiles a
	// configuration. Both report their diagnostics as tapyr validate does.
	Load    func() (*config.Config, error)
	Compile func(*config.Config, runtime.Options) (*runtime.Graph, error)
	// UI holds the assets of the web UI, served unless the settings disable
	// it.
	UI fs.FS
	// Reload receives the reload requests (SIGHUP).
	Reload <-chan struct{}
	// OnReload, when set, observes the outcome of every reload; OnRun the
	// work in flight (+1 before a run, -1 after, and the same for the
	// projection check that follows a run); OnInstances the number of
	// scheduled instances (tests).
	OnReload    func(err error)
	OnRun       func(delta int)
	OnInstances func(n int)
}

// Collector is the process of tapyr run: what outlives a configuration
// reload (state store, series registry, outputs, token sessions, HTTP
// server) and the current generation, the pipeline of one compiled graph.
type Collector struct {
	opts   Options
	clock  clock.Clock
	store  state.Store
	logger *slog.Logger
	// redact removes the secrets and tokens from a text that leaves the
	// process (run errors, reload errors), as the log handler does.
	redact  func(string) string
	tokens  *auth.Manager
	secrets *config.SecretSource
	salt    []byte
	// instance is the persisted identifier of this instance.
	instance string
	// client is shared by the generations, so that a reload does not leave
	// idle connections behind.
	client *http.Client
	// tlsClients are the clients of the accounts with TLS settings, by
	// account, kept across reloads while their settings do not change.
	tlsMu      sync.Mutex
	tlsClients map[string]*accountTLSClient
	series     *series.Registry
	accum      *series.Accumulators
	mux        *http.ServeMux
	metrics    *selfobs.Metrics
	// runs numbers the runs of every generation.
	runs atomic.Uint64
	// heartbeat is the last tick of the flush loop (unix nanoseconds on the
	// injected clock), for /healthz.
	heartbeat atomic.Int64
	// outputsStarted is set once the outputs run, for /readyz.
	outputsStarted atomic.Bool

	// events streams the events of the collector to the UI.
	events *api.Broker
	// debug keeps the last runs for the live debug endpoint.
	debug *debugCaptures
	// circuits are the last circuit states seen, by account and target,
	// to publish their changes.
	cmu      sync.Mutex
	circuits map[string]int

	// smu guards what the status API reports of the process.
	smu        sync.Mutex
	started    time.Time
	lastReload api.Reload
	configHash string

	// settings are those of the running configuration (under gmu);
	// selfChanged is closed when they change.
	settings    *config.SettingsSpec
	selfChanged chan struct{}

	omu     sync.RWMutex
	outputs map[string]*runningOutput
	prom    *prometheus.Output

	gmu sync.Mutex
	gen *generation
}

// New returns a collector; Run starts it.
func New(opts Options) (*Collector, error) {
	redactor := opts.Redactor
	c := &Collector{
		opts: opts, clock: opts.Clock, store: opts.Store, logger: opts.Logger, secrets: opts.Secrets, salt: opts.Salt,
		instance: opts.InstanceID, client: &http.Client{}, series: series.New(), accum: series.NewAccumulators(),
		outputs: map[string]*runningOutput{}, mux: http.NewServeMux(), events: api.NewBroker(opts.Clock),
		debug: newDebugCaptures(redactor.Redact), circuits: map[string]int{}, redact: redactor.Redact,
	}
	c.metrics = selfobs.NewMetrics(buildinfo.Get(), c.selfState)
	tokens, err := auth.NewManager(auth.ManagerOptions{
		Clock: opts.Clock, Logger: opts.Logger, Store: opts.Store, StateKey: opts.StateKey, Observer: c.metrics,
		OnToken: func(token string) { redactor.Add(token) }, Plugins: opts.Plugins,
	})
	if err != nil {
		return nil, err
	}
	c.tokens = tokens
	return c, nil
}

// Compile compiles a configuration for this process: with the credential
// salt and the shared token sessions.
func (c *Collector) Compile(cfg *config.Config) (*runtime.Graph, error) {
	return c.opts.Compile(cfg, runtime.Options{Secrets: c.secrets, CredentialSalt: c.salt, Auth: c.tokens})
}

// Run collects until ctx is done, reloading the configuration on request.
func (c *Collector) Run(ctx context.Context, cfg *config.Config, graph *runtime.Graph) error {
	entries, err := c.store.LoadLedger(ctx)
	if err != nil {
		return err
	}
	snaps, err := c.store.LoadSeries(ctx)
	if err != nil {
		return err
	}
	if skipped := c.series.Restore(snaps); skipped > 0 {
		c.logger.Warn("invalid series keys skipped in the state", "component", "state", "skipped", skipped)
	}
	accs, err := c.store.LoadAccumulators(ctx)
	if err != nil {
		return err
	}
	c.accum.Restore(accs)
	c.settings, c.selfChanged = graph.Settings, make(chan struct{})
	c.started = c.clock.Now()
	c.heartbeat.Store(c.started.UnixNano())
	c.mux.HandleFunc("GET /metrics", c.serveMetrics)
	c.mux.Handle("GET /metrics/self", c.metrics.Handler())
	c.mux.HandleFunc("GET /healthz", c.serveHealth)
	c.mux.HandleFunc("GET /readyz", c.serveReady)
	c.mux.Handle("/api/", api.Handler(statusSource{c}, c.events))
	if e := graph.Settings.Server.UI.Enabled; e == nil || *e {
		c.mux.Handle("GET "+internalui.Prefix, internalui.Handler(c.opts.UI))
		c.mux.Handle("GET /{$}", http.RedirectHandler(internalui.Prefix, http.StatusFound))
	}
	c.debug.configure(graph.Settings.Server.Debug.Enabled, graph.Settings.Server.Debug.MaxBodyBytes)
	ln, err := c.opts.Listen(ctx, "tcp", graph.Settings.Server.Listen)
	if err != nil {
		return err
	}
	c.applyOutputs(ctx, graph)
	c.outputsStarted.Store(true)
	g, err := c.newGeneration(cfg, graph)
	if err != nil {
		return err
	}
	restored := g.engine.Restore(entries)
	c.start(ctx, g, nil, reasonStart)
	c.metrics.ConfigLoaded(c.clock.Now(), true)
	c.recordLoad(cfg, nil)
	active, dynamic := g.disc.counts()
	c.logger.Info("collector started", "component", "run", "address", ln.Addr().String(),
		"instances", active, "discovered_instances", dynamic, "outputs", len(c.outputs), "ledger_entries", restored)

	loops := sync.WaitGroup{}
	loops.Go(func() { c.flushLoop(ctx) })
	loops.Go(func() { c.reloadLoop(ctx) })
	loops.Go(func() { c.selfPushLoop(ctx) })
	handler, err := c.serverHandler(graph.Settings.Server)
	if err != nil {
		return err
	}
	certFile, keyFile := "", ""
	if tls := graph.Settings.Server.TLS; tls != nil {
		certFile, keyFile = tls.CertFile, tls.KeyFile
	}
	serveErr := Serve(ctx, ln, handler, certFile, keyFile)
	loops.Wait()

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), runsShutdownTimeout)
	defer cancel()
	c.current().stop(shutdownCtx, "runs canceled on shutdown")
	c.tokens.Close(shutdownCtx)
	c.stopOutputs(shutdownCtx, slices.Collect(maps.Keys(c.outputs)))
	c.flushLedger(shutdownCtx)
	c.logger.Info("collector stopped", "component", "run")
	if serveErr != nil {
		return serveErr
	}
	return nil
}

// serverHandler wraps the mux with the basic auth of the settings.
func (c *Collector) serverHandler(s config.ServerSettings) (http.Handler, error) {
	ba := s.BasicAuth
	if ba == nil {
		return c.mux, nil
	}
	user, err := secret.Resolve(ba.Username, *c.secrets)
	if err != nil {
		return nil, fmt.Errorf("server.basicAuth.username: %w", err)
	}
	pass, err := secret.Resolve(ba.Password, *c.secrets)
	if err != nil {
		return nil, fmt.Errorf("server.basicAuth.password: %w", err)
	}
	return basicAuth(c.mux, user.Reveal(), pass.Reveal(), ba.ProtectMetrics), nil
}

// serveMetrics serves the exposition of the prometheus output, if any.
func (c *Collector) serveMetrics(w http.ResponseWriter, r *http.Request) {
	c.omu.RLock()
	prom := c.prom
	c.omu.RUnlock()
	if prom == nil {
		http.NotFound(w, r)
		return
	}
	prom.Handler().ServeHTTP(w, r)
}

// accountTLSClient is the HTTP client of an account with TLS settings.
type accountTLSClient struct {
	fingerprint string
	client      *http.Client
}

// accountClients returns the clients of the accounts of a graph that have
// TLS settings, reusing those whose settings did not change, and logs the
// accounts that disable certificate verification.
func (c *Collector) accountClients(graph *runtime.Graph) map[string]*http.Client {
	c.tlsMu.Lock()
	defer c.tlsMu.Unlock()
	next := map[string]*accountTLSClient{}
	out := map[string]*http.Client{}
	for _, account := range slices.Sorted(maps.Keys(graph.TLS)) {
		t := graph.TLS[account]
		if t.Insecure {
			c.logger.Warn("TLS certificate verification is disabled (insecureSkipVerify)", "component", "run", "account", account)
		}
		tc, ok := c.tlsClients[account]
		if !ok || tc.fingerprint != t.Fingerprint {
			tc = &accountTLSClient{fingerprint: t.Fingerprint, client: TLSClient(t)}
		}
		next[account], out[account] = tc, tc.client
	}
	for account, tc := range c.tlsClients {
		if next[account] != tc {
			tc.client.CloseIdleConnections()
		}
	}
	c.tlsClients = next
	return out
}

func (c *Collector) current() *generation {
	c.gmu.Lock()
	defer c.gmu.Unlock()
	return c.gen
}

// reloadLoop reloads the configuration on every request (SIGHUP) until ctx
// is done.
func (c *Collector) reloadLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.opts.Reload:
			err := c.reload(ctx)
			c.metrics.ConfigLoaded(c.clock.Now(), err == nil)
			if err != nil {
				c.recordLoad(nil, err)
				c.logger.Error("configuration reload rejected: the running configuration is kept", "component", "run", "error", err)
				c.events.Publish(api.EventConfigReloaded, map[string]any{"result": "failed", "error": c.redact(err.Error())})
			} else {
				c.events.Publish(api.EventConfigReloaded, map[string]any{"result": "success"})
			}
			if c.opts.OnReload != nil {
				c.opts.OnReload(err)
			}
		}
	}
}

// reload loads and compiles the configuration, then swaps generations: the
// old one stops scheduling and finishes its runs, the ledger moves to the
// new engine, unchanged outputs keep running, instances that disappeared
// are retired and the others keep their schedule.
func (c *Collector) reload(ctx context.Context) error {
	cfg, err := c.opts.Load()
	if err != nil {
		return err
	}
	graph, err := c.Compile(cfg)
	if err != nil {
		return err
	}
	next, err := c.newGeneration(cfg, graph)
	if err != nil {
		return err
	}
	c.warnRestartSettings(graph.Settings)

	old := c.current()
	stopCtx, cancel := context.WithTimeout(ctx, runsShutdownTimeout)
	defer cancel()
	old.stop(stopCtx, "runs canceled on reload")
	before := old.disc.activeKeys()
	resume := old.sched.Next()
	next.engine.Restore(old.engine.Export())
	c.applyOutputs(ctx, graph)
	c.start(ctx, next, resume, reasonReload)
	after := next.disc.activeKeys()
	retired := 0
	for key := range before {
		if !after[key] {
			c.retire(key, c.series.Remove(key))
			retired++
		}
	}
	c.recordLoad(cfg, nil)
	c.debug.configure(graph.Settings.Server.Debug.Enabled, graph.Settings.Server.Debug.MaxBodyBytes)
	c.gmu.Lock()
	c.settings = graph.Settings
	close(c.selfChanged)
	c.selfChanged = make(chan struct{})
	c.gmu.Unlock()
	active, dynamic := next.disc.counts()
	c.logger.Info("configuration reloaded", "component", "run", "instances", active, "discovered_instances", dynamic,
		"retired_instances", retired, "outputs", len(c.outputs))
	return nil
}

// warnRestartSettings logs the settings changes that only a restart applies.
func (c *Collector) warnRestartSettings(next *config.SettingsSpec) {
	for name, changed := range map[string]bool{
		"server":     !reflect.DeepEqual(restartServer(c.settings.Server), restartServer(next.Server)),
		"state.path": c.settings.State.Path != next.State.Path,
		"log":        c.settings.Log != next.Log,
	} {
		if changed {
			c.logger.Warn("settings changed that take effect after a restart", "component", "run", "settings", name)
		}
	}
}

// restartServer returns the server settings that only a restart applies:
// all of them but the live debug, which a reload applies.
func restartServer(s config.ServerSettings) config.ServerSettings {
	s.Debug = config.DebugSettings{}
	return s
}

// flushLoop persists the ledger and the series, and expires stale
// inventories and series, every budget.FlushInterval until ctx is done.
func (c *Collector) flushLoop(ctx context.Context) {
	ticker := c.clock.NewTicker(budget.FlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C():
			// A shutdown does not interrupt a flush: the final one follows.
			fctx := context.WithoutCancel(ctx)
			now := c.clock.Now()
			c.heartbeat.Store(now.UnixNano())
			g := c.current()
			g.disc.expire(fctx, now)
			c.expireSeries(g, now)
			c.flushLedger(fctx)
		}
	}
}

// flushLedger persists the ledger of the current engine and the series
// registry.
func (c *Collector) flushLedger(ctx context.Context) {
	started := c.clock.Now()
	err := c.store.SaveLedger(ctx, c.current().engine.Export())
	if err != nil {
		c.logger.Warn("persisting the ledger", "component", "state", "error", err)
	}
	if changed, removed := c.series.Snapshot(); len(changed)+len(removed) > 0 {
		if serr := c.store.SaveSeries(ctx, changed, removed); serr != nil {
			c.logger.Warn("persisting the series", "component", "state", "error", serr)
			err = errors.Join(err, serr)
		}
	}
	if changed, removed := c.accum.Snapshot(); len(changed)+len(removed) > 0 {
		if aerr := c.store.SaveAccumulators(ctx, changed, removed); aerr != nil {
			c.logger.Warn("persisting the reconstructed counters", "component", "state", "error", aerr)
			err = errors.Join(err, aerr)
		}
	}
	c.metrics.StateFlushed(c.clock.Since(started), err)
}

// expireSeries withdraws the series of the instances without a successful
// run within their staleAfter, and of the instances no longer scheduled.
func (c *Collector) expireSeries(g *generation, now time.Time) {
	g.disc.do(func(active map[string]*runtime.Job) {
		expired := c.series.Expired(now, func(instance string) (time.Duration, bool) {
			j, ok := active[instance]
			if !ok {
				return 0, false
			}
			return g.disc.staleAfter(j), true
		})
		for _, instance := range expired {
			j, ok := active[instance]
			if !ok {
				c.retire(instance, c.series.Remove(instance))
				continue
			}
			stale := c.series.Expire(instance)
			c.logger.Warn("series withdrawn: no successful run within staleAfter", "component", "series", "provider", j.Provider,
				"account", j.Account, "job", j.Name, "instance", instance, "series", len(stale),
				"stale_after", g.disc.staleAfter(j).String())
			c.retire(instance, stale)
		}
	})
}

// retire withdraws the series of an instance from every output; its
// reconstructed counters start again.
func (c *Collector) retire(instance string, stale []model.Series) {
	c.accum.Remove(instance)
	c.debug.remove(instance)
	c.omu.RLock()
	defer c.omu.RUnlock()
	for _, ro := range c.outputs {
		ro.out.Retire(instance, stale)
	}
}
