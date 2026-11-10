package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/kentrow/tapyr/internal/clock"
	"github.com/kentrow/tapyr/internal/config"
	"github.com/kentrow/tapyr/internal/secret"
)

// Plugin is an auth flow written in Go, for the APIs whose auth cannot be
// declared (see docs/spec/05-collection.md, "Plugins"). A plugin is
// selected with auth.type plugin and receives auth.plugin.config.
type Plugin interface {
	// Name is the name used in auth.plugin.name.
	Name() string
	// Validate checks a configuration at compile time and returns the
	// account secrets it references ({secret: name} values). New reports
	// the required ones an account does not provide.
	Validate(config map[string]any) (secrets []string, err error)
	// New returns the authenticator of an account (or target).
	New(env PluginEnv, config map[string]any) (PluginAuthenticator, error)
	// Simulator returns the simulator side of the plugin, which serves its
	// token endpoints and checks the tokens it issued. accept is
	// auth.accept.plugin of the Simulation (free-form, fake credentials).
	Simulator(env SimulatorEnv, accept map[string]any) (PluginSimulator, error)
}

// PluginAuthenticator is the authenticator of a plugin.
type PluginAuthenticator interface {
	Authenticator
	// Status reports the expiry of the current token (zero when none) and
	// whether a manual re-bootstrap is required.
	Status() (expiry time.Time, reauth bool)
}

// PluginEnv is what a plugin authenticator may use.
type PluginEnv struct {
	Scope   Scope
	BaseURL *url.URL
	Secrets Secrets
	Clock   clock.Clock
	Logger  *slog.Logger
	// Send sends an auth request (token requests) through the budget
	// engine; endpoint is its path.
	Send func(ctx context.Context, endpoint string, req *http.Request) (*http.Response, error)
	// Store persists the state of the plugin for the account, sealed like
	// tokens (TAPYR_STATE_KEY).
	Store PluginStore
	// Observe records a token request: result is success or failed.
	Observe func(result string)
	// OnToken registers a token for log redaction.
	OnToken func(token string)
	// ReadOnly is set for commands that run next to tapyr run (tapyr
	// record): Store does not persist, and the plugin must not renew
	// credentials whose renewal invalidates the previous ones (a rotating
	// refresh token), or tapyr run would be left with spent ones. It returns
	// ErrRenewalNotAllowed instead.
	ReadOnly bool
}

// PluginStore persists the state of a plugin for an account.
type PluginStore interface {
	Load(ctx context.Context) (value string, ok bool)
	// Save stores value; it returns an error so that a plugin can refuse
	// to use credentials it could not persist.
	Save(ctx context.Context, value string, expiry time.Time) error
	Delete(ctx context.Context)
}

// SimulatorEnv is what the simulator side of a plugin may use.
type SimulatorEnv struct {
	Clock clock.Clock
	// BasePath is the path prefix of the simulated API.
	BasePath string
	// TokenLifetime is auth.tokenLifetime of the Simulation (0 for the
	// plugin default).
	TokenLifetime time.Duration
}

// PluginSimulator serves the token endpoints of a plugin in the simulator.
// The simulator applies the provider limits to its requests.
type PluginSimulator interface {
	// TokenRequest reports whether req is a request for a token endpoint
	// of the plugin, its endpoint path, and the identity of the client
	// when its credentials are accepted.
	TokenRequest(req *http.Request) (endpoint, identity string, handled, ok bool)
	// ServeToken answers an accepted token request.
	ServeToken(w http.ResponseWriter, req *http.Request, identity string)
	// Identity returns the identity of the token an API request carries.
	Identity(req *http.Request) (string, bool)
	// ExpireBefore invalidates the tokens issued before t.
	ExpireBefore(t time.Time)
	// Issued counts the tokens issued.
	Issued() int64
}

// Plugins is a registry of plugins by name.
type Plugins map[string]Plugin

// NewPlugins returns a registry of plugins.
func NewPlugins(ps ...Plugin) Plugins {
	out := Plugins{}
	for _, p := range ps {
		out[p.Name()] = p
	}
	return out
}

// Get returns a plugin, or an error naming the registered ones.
func (ps Plugins) Get(name string) (Plugin, error) {
	if p, ok := ps[name]; ok {
		return p, nil
	}
	names := make([]string, 0, len(ps))
	for n := range ps {
		names = append(names, n)
	}
	slices.Sort(names)
	return nil, fmt.Errorf("unknown auth plugin %q (available: %s)", name, strings.Join(names, ", "))
}

// ValidatePlugin checks the configuration of a plugin auth block and
// returns the secrets it references.
func (m *Manager) ValidatePlugin(a *config.PluginAuth) ([]string, error) {
	p, err := m.plugins.Get(a.Name)
	if err != nil {
		return nil, err
	}
	return p.Validate(a.Config)
}

// ErrReauthRequired is returned by plugins whose credentials can no longer
// be renewed: the account needs a manual re-bootstrap.
var ErrReauthRequired = errors.New("the credentials can no longer be renewed: a manual re-bootstrap is required")

// ErrRenewalNotAllowed is returned by plugins in read-only mode when the
// credentials need a renewal that would invalidate those of tapyr run.
var ErrRenewalNotAllowed = errors.New("the access token must be renewed, which would spend the refresh token that tapyr run uses: " +
	"retry while tapyr run holds a valid access token, or with a token pair of its own")

// pluginSession is a plugin authenticator shared by the jobs of an
// account.
type pluginSession struct {
	scope Scope
	auth  PluginAuthenticator
}

// plugin returns the authenticator of a plugin auth block, shared per
// account, credential, configuration and secret values (a changed secret
// opens a new session).
func (m *Manager) plugin(a *config.PluginAuth, s Session) (Authenticator, error) {
	p, err := m.plugins.Get(a.Name)
	if err != nil {
		return nil, err
	}
	refs, err := p.Validate(a.Config)
	if err != nil {
		return nil, err
	}
	// The plugin checks the secrets it requires; optional ones may be
	// missing.
	h := sha256.New()
	for _, name := range refs {
		if v, ok := s.Secrets[name]; ok && !v.IsZero() {
			fmt.Fprintf(h, "%s=%s\n", name, v.Reveal())
		}
	}
	base := ""
	if s.BaseURL != nil {
		base = s.BaseURL.String()
	}
	storeKey := strings.Join([]string{"plugin", a.Name, s.Scope.Provider, s.Scope.Account, fingerprint(a, base)}, "|")
	key := storeKey + "|" + hex.EncodeToString(h.Sum(nil)[:8])
	m.mu.Lock()
	defer m.mu.Unlock()
	if ps, ok := m.pluginSessions[key]; ok {
		return ps.auth, nil
	}
	scope := s.Scope
	env := PluginEnv{
		Scope: scope, BaseURL: s.BaseURL, Secrets: s.Secrets, Clock: m.clock,
		Logger: m.logger.With("component", "auth", "plugin", a.Name, "provider", scope.Provider, "account", scope.Account),
		Send: func(ctx context.Context, endpoint string, req *http.Request) (*http.Response, error) {
			return m.send(ctx, scope, endpoint, req)
		},
		Store:    &pluginStore{m: m, key: storeKey},
		Observe:  func(result string) { m.obs.TokenRequest(scope.Provider, scope.Account, result) },
		OnToken:  m.onTok,
		ReadOnly: m.readOnly,
	}
	pa, err := p.New(env, a.Config)
	if err != nil {
		return nil, fmt.Errorf("auth plugin %s: %w", a.Name, err)
	}
	m.pluginSessions[key] = &pluginSession{scope: scope, auth: pa}
	return pa, nil
}

// pluginStore persists the state of a plugin in the token store.
type pluginStore struct {
	m   *Manager
	key string
	mu  sync.Mutex
}

func (s *pluginStore) Load(ctx context.Context) (string, bool) {
	v, _, ok := s.m.load(ctx, s.key)
	if !ok {
		return "", false
	}
	return v.Reveal(), true
}

func (s *pluginStore) Save(ctx context.Context, value string, expiry time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m.store == nil || s.m.readOnly {
		return nil
	}
	sealed, err := s.m.seal(s.key, value)
	if err == nil {
		err = s.m.store.SaveToken(ctx, s.key, sealed, expiry)
	}
	return err
}

func (s *pluginStore) Delete(ctx context.Context) { s.m.forget(ctx, s.key) }

// pluginSecret resolves a {secret: name} plugin config value.
func pluginSecret(v any) (string, bool) {
	m, ok := v.(map[string]any)
	if !ok || len(m) != 1 {
		return "", false
	}
	name, ok := m["secret"].(string)
	return name, ok && name != ""
}

// PluginSecret returns the secret named by a {secret: name} plugin config
// value.
func PluginSecret(config map[string]any, key string, secrets Secrets) (secret.Value, error) {
	name, ok := pluginSecret(config[key])
	if !ok {
		return secret.Value{}, fmt.Errorf("%s must be a secret reference ({secret: name})", key)
	}
	v, ok := secrets[name]
	if !ok || v.IsZero() {
		return secret.Value{}, fmt.Errorf("secret %q is not provided by the account", name)
	}
	return v, nil
}

// PluginSecretName returns the secret named by a {secret: name} plugin
// config value, for Validate.
func PluginSecretName(config map[string]any, key string) (string, error) {
	name, ok := pluginSecret(config[key])
	if !ok {
		return "", fmt.Errorf("%s must be a secret reference ({secret: name})", key)
	}
	return name, nil
}
