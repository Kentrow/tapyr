// Package arubacentral is the aruba_central_classic auth plugin: OAuth
// access tokens of HPE Aruba Networking Central (classic platform), renewed
// with rotating refresh tokens (see docs/spec/05-collection.md, "Plugins").
package arubacentral

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/kentrow/tapyr/internal/auth"
	"github.com/kentrow/tapyr/internal/secret"
)

// Name of the plugin in auth.plugin.name.
const Name = "aruba_central_classic"

const (
	defaultTokenPath     = "/oauth2/token" //nolint:gosec // G101: an endpoint path, not a credential.
	defaultRefreshBefore = 10 * time.Minute
	// defaultLifetime applies when a refresh answers no expires_in: the
	// documented 2 hours.
	defaultLifetime = 2 * time.Hour
	// refreshLifetime is the documented lifetime of a refresh token.
	refreshLifetime = 15 * 24 * time.Hour
	maxResponse     = 1 << 20
)

// Plugin is the aruba_central_classic plugin.
type Plugin struct{}

// New returns the plugin.
func New() Plugin { return Plugin{} }

// Name implements auth.Plugin.
func (Plugin) Name() string { return Name }

// settings is the parsed plugin configuration.
type settings struct {
	clientID, clientSecret, refreshToken, accessToken string // secret names
	tokenPath                                         string
	refreshBefore                                     time.Duration
}

func parse(cfg map[string]any) (*settings, error) {
	s := &settings{tokenPath: defaultTokenPath, refreshBefore: defaultRefreshBefore}
	known := map[string]bool{"clientId": true, "clientSecret": true, "refreshToken": true, "accessToken": true, "tokenPath": true, "refreshBefore": true}
	for k := range cfg {
		if !known[k] {
			return nil, fmt.Errorf("unknown config field %q", k)
		}
	}
	var err error
	for _, f := range []struct {
		key string
		dst *string
	}{{"clientId", &s.clientID}, {"clientSecret", &s.clientSecret}, {"refreshToken", &s.refreshToken}} {
		if *f.dst, err = auth.PluginSecretName(cfg, f.key); err != nil {
			return nil, err
		}
	}
	if _, ok := cfg["accessToken"]; ok {
		if s.accessToken, err = auth.PluginSecretName(cfg, "accessToken"); err != nil {
			return nil, err
		}
	}
	if v, ok := cfg["tokenPath"]; ok {
		p, isString := v.(string)
		if !isString || !strings.HasPrefix(p, "/") {
			return nil, errors.New("tokenPath must be a path starting with /")
		}
		s.tokenPath = p
	}
	if v, ok := cfg["refreshBefore"]; ok {
		str, isString := v.(string)
		d, err := time.ParseDuration(str)
		if !isString || err != nil || d < 0 || d >= defaultLifetime {
			return nil, errors.New("refreshBefore must be a duration below 2h, for example 10m")
		}
		s.refreshBefore = d
	}
	return s, nil
}

// Validate implements auth.Plugin.
func (Plugin) Validate(cfg map[string]any) ([]string, error) {
	s, err := parse(cfg)
	if err != nil {
		return nil, err
	}
	refs := []string{s.clientID, s.clientSecret, s.refreshToken}
	if s.accessToken != "" {
		refs = append(refs, s.accessToken)
	}
	return refs, nil
}

// state is the token pair persisted in the state store.
type state struct {
	Access  string    `json:"access_token"`
	Refresh string    `json:"refresh_token"`
	Expiry  time.Time `json:"expiry"`
	// Bootstrap is a hash of the bootstrap refresh token the pair descends
	// from: a changed bootstrap secret replaces the pair.
	Bootstrap string `json:"bootstrap"`
	// Reauth is set when the refresh token was rejected.
	Reauth bool `json:"reauth,omitempty"`
}

// New implements auth.Plugin.
func (Plugin) New(env auth.PluginEnv, cfg map[string]any) (auth.PluginAuthenticator, error) {
	s, err := parse(cfg)
	if err != nil {
		return nil, err
	}
	a := &authenticator{env: env, settings: s}
	if a.clientID, err = auth.PluginSecret(cfg, "clientId", env.Secrets); err != nil {
		return nil, err
	}
	if a.clientSecret, err = auth.PluginSecret(cfg, "clientSecret", env.Secrets); err != nil {
		return nil, err
	}
	if a.bootRefresh, err = auth.PluginSecret(cfg, "refreshToken", env.Secrets); err != nil {
		return nil, err
	}
	if v, ok := env.Secrets[s.accessToken]; s.accessToken != "" && ok && !v.IsZero() {
		a.bootAccess = v // optional
	}
	sum := sha256.Sum256([]byte(a.bootRefresh.Reveal()))
	a.bootHash = hex.EncodeToString(sum[:])
	if env.BaseURL == nil {
		return nil, errors.New("no base URL")
	}
	u := *env.BaseURL
	u.Path = strings.TrimSuffix(u.Path, "/") + s.tokenPath
	a.tokenURL = &u
	return a, nil
}

// authenticator renews the access token with the refresh token. Refreshes
// are serialized: the lock is held during a refresh.
type authenticator struct {
	env      auth.PluginEnv
	settings *settings
	tokenURL *url.URL

	clientID, clientSecret, bootRefresh, bootAccess secret.Value
	bootHash                                        string

	mu     sync.Mutex
	loaded bool
	st     state
}

// Status implements auth.PluginAuthenticator.
func (a *authenticator) Status() (time.Time, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.st.Access == "" {
		return time.Time{}, a.st.Reauth
	}
	return a.st.Expiry, a.st.Reauth
}

// loadLocked reads the stored pair, or starts from the bootstrap secrets
// when none is stored or the bootstrap refresh token changed.
func (a *authenticator) loadLocked(ctx context.Context) {
	if a.loaded {
		return
	}
	a.loaded = true
	if raw, ok := a.env.Store.Load(ctx); ok {
		var st state
		if err := json.Unmarshal([]byte(raw), &st); err == nil && st.Bootstrap == a.bootHash {
			a.st = st
			a.env.OnToken(st.Access)
			a.env.OnToken(st.Refresh)
			return
		}
		a.env.Logger.Info("the bootstrap refresh token changed: the stored token pair is replaced")
	}
	a.st = state{Refresh: a.bootRefresh.Reveal(), Bootstrap: a.bootHash}
	if !a.bootAccess.IsZero() {
		// Its age is unknown: used until rejected or the next refresh.
		a.st.Access = a.bootAccess.Reveal()
		a.st.Expiry = a.env.Clock.Now().Add(a.settings.refreshBefore + time.Minute)
	}
}

// Decorate implements auth.Authenticator.
func (a *authenticator) Decorate(ctx context.Context, req *http.Request) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.loadLocked(ctx)
	if a.st.Reauth {
		return auth.ErrReauthRequired
	}
	switch {
	case a.env.ReadOnly:
		// A refresh would spend the refresh token of tapyr run: the access
		// token is used until its expiry, and never renewed.
		if a.st.Access == "" || !a.env.Clock.Now().Before(a.st.Expiry) {
			return auth.ErrRenewalNotAllowed
		}
	case a.st.Access == "" || !a.env.Clock.Now().Before(a.st.Expiry.Add(-a.settings.refreshBefore)):
		if err := a.refreshLocked(ctx); err != nil {
			return err
		}
	}
	req.Header.Set("Authorization", "Bearer "+a.st.Access)
	return nil
}

// OnAuthFailure implements auth.Authenticator: the access token is dropped
// and the request retried once after a refresh (not in read-only mode).
func (a *authenticator) OnAuthFailure(context.Context, *http.Response) (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.st.Reauth {
		return false, nil
	}
	a.st.Access = ""
	if a.env.ReadOnly {
		a.env.Logger.Warn("the access token was rejected; it is not renewed in read-only mode", "error", auth.ErrRenewalNotAllowed)
		return false, nil
	}
	return true, nil
}

// refreshLocked exchanges the refresh token for a new pair, and writes the
// pair to the store before using it: the previous refresh token is spent.
func (a *authenticator) refreshLocked(ctx context.Context) error {
	q := url.Values{
		"client_id": {a.clientID.Reveal()}, "client_secret": {a.clientSecret.Reveal()},
		"grant_type": {"refresh_token"}, "refresh_token": {a.st.Refresh},
	}
	u := *a.tokenURL
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), http.NoBody)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.env.Send(ctx, a.settings.tokenPath, req)
	if err != nil {
		a.env.Observe("failed")
		return fmt.Errorf("refreshing the access token: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		a.env.Observe("failed")
		return fmt.Errorf("reading the refresh response: %w", err)
	}
	switch {
	case resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized:
		a.env.Observe("failed")
		a.st.Access, a.st.Reauth = "", true
		if err := a.save(ctx); err != nil {
			a.env.Logger.Warn("storing the re-bootstrap condition", "error", err)
		}
		a.env.Logger.Error("the refresh token was rejected: generate a new token pair in the Central API Gateway and update the bootstrap secrets",
			"status", resp.StatusCode)
		return fmt.Errorf("refresh answered %d: %w", resp.StatusCode, auth.ErrReauthRequired)
	case resp.StatusCode != http.StatusOK:
		a.env.Observe("failed")
		return fmt.Errorf("refresh answered %d", resp.StatusCode)
	}
	var tr struct {
		AccessToken  string          `json:"access_token"`
		RefreshToken string          `json:"refresh_token"`
		ExpiresIn    json.RawMessage `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tr); err != nil || tr.AccessToken == "" || tr.RefreshToken == "" {
		a.env.Observe("failed")
		return errors.New("refresh: no access_token or refresh_token in the response")
	}
	a.env.OnToken(tr.AccessToken)
	a.env.OnToken(tr.RefreshToken)
	lifetime := defaultLifetime
	var secs float64
	if err := json.Unmarshal(tr.ExpiresIn, &secs); err == nil && secs > 0 {
		lifetime = time.Duration(secs * float64(time.Second))
	}
	a.st.Access, a.st.Refresh, a.st.Expiry = tr.AccessToken, tr.RefreshToken, a.env.Clock.Now().Add(lifetime)
	if err := a.save(ctx); err != nil {
		// The previous refresh token is spent: keep the new pair in memory
		// and try to store it at the next refresh.
		a.env.Logger.Error("storing the rotated token pair failed: a restart before the next refresh needs a re-bootstrap",
			"error", err)
	}
	a.env.Observe("success")
	return nil
}

func (a *authenticator) save(ctx context.Context) error {
	data, err := json.Marshal(a.st) //nolint:gosec // G117: sealed by the token store, never logged.
	if err != nil {
		return err
	}
	return a.env.Store.Save(ctx, string(data), a.env.Clock.Now().Add(refreshLifetime))
}
