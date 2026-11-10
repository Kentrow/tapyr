package arubacentral

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kentrow/tapyr/internal/auth"
	"github.com/kentrow/tapyr/internal/clock"
	"github.com/kentrow/tapyr/internal/config"
	"github.com/kentrow/tapyr/internal/secret"
)

// memStore is an in-memory token store.
type memStore struct {
	mu     sync.Mutex
	tokens map[string][]byte
	fail   bool
}

func (s *memStore) LoadToken(_ context.Context, key string) ([]byte, time.Time, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.tokens[key]
	return v, time.Time{}, ok, nil
}

func (s *memStore) SaveToken(_ context.Context, key string, sealed []byte, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail {
		return errors.New("disk full")
	}
	s.tokens[key] = sealed
	return nil
}

func (s *memStore) DeleteToken(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tokens, key)
	return nil
}

// stored returns the token pair of the store (there is one account).
func (s *memStore) stored(t *testing.T) state {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range s.tokens {
		var st state
		if err := json.Unmarshal(v[1:], &st); err != nil { // version byte, then the plain value
			t.Fatal(err)
		}
		return st
	}
	t.Fatal("nothing stored")
	return state{}
}

type sender struct{}

func (sender) SendAuth(_ context.Context, _ auth.Scope, _ string, req *http.Request) (*http.Response, error) {
	return http.DefaultClient.Do(req)
}

// central serves the plugin simulator and an API answering 200 to valid
// access tokens; reject makes the refresh endpoint answer 503.
type central struct {
	srv    *httptest.Server
	sim    auth.PluginSimulator
	reject bool
}

func newCentral(t *testing.T, clk clock.Clock) *central {
	t.Helper()
	sim, err := New().Simulator(auth.SimulatorEnv{Clock: clk}, map[string]any{
		"clients":       []any{map[string]any{"clientId": "app", "clientSecret": "s3cr3t"}},
		"refreshTokens": []any{"boot-refresh", "other-refresh"},
		"accessTokens":  []any{"boot-access"},
	})
	if err != nil {
		t.Fatal(err)
	}
	c := &central{sim: sim}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, identity, handled, ok := sim.TokenRequest(r); handled {
			if c.reject {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			if !ok {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			sim.ServeToken(w, r, identity)
			return
		}
		if _, ok := sim.Identity(r); !ok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(c.srv.Close)
	return c
}

// call sends an API request like the executor: one retry after an auth
// failure.
func call(t *testing.T, a auth.Authenticator, base string) (int, error) {
	t.Helper()
	for attempt := 0; ; attempt++ {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, base+"/monitoring/v2/aps", http.NoBody)
		if err := a.Decorate(context.Background(), req); err != nil {
			return 0, err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized || attempt > 0 {
			return resp.StatusCode, nil
		}
		if retry, _ := a.OnAuthFailure(context.Background(), resp); !retry {
			return resp.StatusCode, nil
		}
	}
}

var pluginConfig = map[string]any{
	"clientId": map[string]any{"secret": "clientId"}, "clientSecret": map[string]any{"secret": "clientSecret"},
	"refreshToken": map[string]any{"secret": "refreshToken"}, "accessToken": map[string]any{"secret": "accessToken"},
}

func session(base, refresh string) auth.Session {
	u, _ := url.Parse(base)
	return auth.Session{
		Scope: auth.Scope{Provider: "aruba", Account: "campus", Credential: "c"}, BaseURL: u,
		Secrets: auth.Secrets{
			"clientId": secret.New("app"), "clientSecret": secret.New("s3cr3t"),
			"refreshToken": secret.New(refresh), "accessToken": secret.New("boot-access"),
		},
	}
}

func manager(t *testing.T, clk clock.Clock, store auth.TokenStore, tokens *[]string) *auth.Manager {
	t.Helper()
	m, err := auth.NewManager(auth.ManagerOptions{
		Clock: clk, Store: store, Plugins: auth.NewPlugins(New()),
		OnToken: func(tok string) { *tokens = append(*tokens, tok) },
	})
	if err != nil {
		t.Fatal(err)
	}
	m.SetSender(sender{})
	return m
}

func TestRotation(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, time.November, 3, 9, 0, 0, 0, time.UTC))
	c := newCentral(t, clk)
	store := &memStore{tokens: map[string][]byte{}}
	var tokens []string
	block := &config.Auth{Type: config.AuthPlugin, Plugin: &config.PluginAuth{Name: Name, Config: pluginConfig}}
	m := manager(t, clk, store, &tokens)
	a, err := m.For(block, session(c.srv.URL, "boot-refresh"))
	if err != nil {
		t.Fatal(err)
	}
	// The bootstrap access token works until it is close to its expiry.
	if status, err := call(t, a, c.srv.URL); err != nil || status != http.StatusOK {
		t.Fatalf("with the bootstrap token: %d %v", status, err)
	}
	if c.sim.Issued() != 0 {
		t.Fatalf("%d tokens issued before they were needed", c.sim.Issued())
	}
	// Past it, the plugin refreshes: the new pair is stored, the bootstrap
	// refresh token is spent.
	clk.Advance(2 * time.Minute)
	if status, err := call(t, a, c.srv.URL); err != nil || status != http.StatusOK {
		t.Fatalf("after the refresh: %d %v", status, err)
	}
	st := store.stored(t)
	if st.Access != "sim-access-1" || st.Refresh != "sim-refresh-1" || st.Reauth {
		t.Fatalf("stored = %+v", st)
	}
	if !strings.Contains(strings.Join(tokens, " "), "sim-refresh-1") {
		t.Error("the new refresh token is not registered for redaction")
	}
	expiry, reauth := a.(auth.PluginAuthenticator).Status()
	if reauth || !expiry.Equal(clk.Now().Add(2*time.Hour)) {
		t.Errorf("Status() = %v, %t", expiry, reauth)
	}
	// Close to the expiry (refreshBefore 10m), the pair rotates again.
	clk.Advance(111 * time.Minute)
	if status, _ := call(t, a, c.srv.URL); status != http.StatusOK || store.stored(t).Refresh != "sim-refresh-2" {
		t.Fatalf("second rotation: %d, %+v", status, store.stored(t))
	}

	// A new process with the same bootstrap secrets resumes from the
	// stored pair (the bootstrap refresh token is spent).
	var tokens2 []string
	m2 := manager(t, clk, store, &tokens2)
	a2, err := m2.For(block, session(c.srv.URL, "boot-refresh"))
	if err != nil {
		t.Fatal(err)
	}
	if status, err := call(t, a2, c.srv.URL); err != nil || status != http.StatusOK {
		t.Fatalf("after a restart: %d %v", status, err)
	}
	if c.sim.Issued() != 2 {
		t.Errorf("%d tokens issued, want 2: the restart refreshed", c.sim.Issued())
	}

	// A rejected access token triggers one refresh.
	c.sim.ExpireBefore(clk.Now())
	if status, err := call(t, a2, c.srv.URL); err != nil || status != http.StatusOK || c.sim.Issued() != 3 {
		t.Fatalf("after a rejection: %d %v, %d issued", status, err, c.sim.Issued())
	}

	// A changed bootstrap secret replaces the stored pair.
	var tokens3 []string
	m3 := manager(t, clk, store, &tokens3)
	a3, err := m3.For(block, session(c.srv.URL, "other-refresh"))
	if err != nil {
		t.Fatal(err)
	}
	c.sim.ExpireBefore(clk.Now())
	if status, err := call(t, a3, c.srv.URL); err != nil || status != http.StatusOK || c.sim.Issued() != 4 {
		t.Fatalf("with a new bootstrap: %d %v, %d issued", status, err, c.sim.Issued())
	}

	// A transient refresh failure is retried at the next request.
	c.sim.ExpireBefore(clk.Now())
	c.reject = true
	if _, err := call(t, a3, c.srv.URL); err == nil || errors.Is(err, auth.ErrReauthRequired) {
		t.Fatalf("transient failure: %v", err)
	}
	c.reject = false
	if status, err := call(t, a3, c.srv.URL); err != nil || status != http.StatusOK {
		t.Fatalf("after a transient failure: %d %v", status, err)
	}
}

// TestReadOnly is a regression test: tapyr record started from the
// bootstrap pair, which tapyr run had already rotated, and tried to refresh
// it. Read-only, the plugin uses the stored pair and never refreshes: a
// refresh would spend the refresh token of tapyr run.
func TestReadOnly(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, time.November, 3, 9, 0, 0, 0, time.UTC))
	c := newCentral(t, clk)
	store := &memStore{tokens: map[string][]byte{}}
	var tokens []string
	block := &config.Auth{Type: config.AuthPlugin, Plugin: &config.PluginAuth{Name: Name, Config: pluginConfig}}
	run, err := manager(t, clk, store, &tokens).For(block, session(c.srv.URL, "boot-refresh"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, run, c.srv.URL); err != nil {
		t.Fatal(err)
	}
	clk.Advance(2 * time.Minute)
	if status, err := call(t, run, c.srv.URL); err != nil || status != http.StatusOK || c.sim.Issued() != 1 {
		t.Fatalf("tapyr run: %d %v, %d issued", status, err, c.sim.Issued())
	}
	stored := store.stored(t)

	rec, err := readOnlyManager(t, clk, store).For(block, session(c.srv.URL, "boot-refresh"))
	if err != nil {
		t.Fatal(err)
	}
	if status, err := call(t, rec, c.srv.URL); err != nil || status != http.StatusOK {
		t.Fatalf("with the stored pair: %d %v", status, err)
	}
	// The access token is used until its expiry, not refreshBefore it.
	clk.Advance(115 * time.Minute)
	if status, err := call(t, rec, c.srv.URL); err != nil || status != http.StatusOK {
		t.Fatalf("before the expiry: %d %v", status, err)
	}
	clk.Advance(5 * time.Minute)
	if _, err := call(t, rec, c.srv.URL); !errors.Is(err, auth.ErrRenewalNotAllowed) {
		t.Fatalf("at the expiry: %v", err)
	}
	if c.sim.Issued() != 1 || store.stored(t) != stored {
		t.Fatalf("the read-only plugin refreshed or wrote: %d issued, %+v", c.sim.Issued(), store.stored(t))
	}
	// tapyr run still holds a valid refresh token.
	if status, err := call(t, run, c.srv.URL); err != nil || status != http.StatusOK || c.sim.Issued() != 2 {
		t.Fatalf("tapyr run after the recording: %d %v, %d issued", status, err, c.sim.Issued())
	}

	// A rejected access token is not refreshed either.
	rec, _ = readOnlyManager(t, clk, store).For(block, session(c.srv.URL, "boot-refresh"))
	c.sim.ExpireBefore(clk.Now())
	if status, err := call(t, rec, c.srv.URL); err != nil || status != http.StatusUnauthorized {
		t.Fatalf("rejected token: %d %v", status, err)
	}
	if _, err := call(t, rec, c.srv.URL); !errors.Is(err, auth.ErrRenewalNotAllowed) || c.sim.Issued() != 2 {
		t.Fatalf("after a rejection: %v, %d issued", err, c.sim.Issued())
	}
}

// TestReadOnlyBootstrap checks that without a stored pair, a read-only
// plugin uses the bootstrap access token and never spends the bootstrap
// refresh token.
func TestReadOnlyBootstrap(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, time.November, 3, 9, 0, 0, 0, time.UTC))
	c := newCentral(t, clk)
	store := &memStore{tokens: map[string][]byte{}}
	block := &config.Auth{Type: config.AuthPlugin, Plugin: &config.PluginAuth{Name: Name, Config: pluginConfig}}
	rec, err := readOnlyManager(t, clk, store).For(block, session(c.srv.URL, "boot-refresh"))
	if err != nil {
		t.Fatal(err)
	}
	if status, err := call(t, rec, c.srv.URL); err != nil || status != http.StatusOK {
		t.Fatalf("with the bootstrap access token: %d %v", status, err)
	}
	clk.Advance(11 * time.Minute)
	if _, err := call(t, rec, c.srv.URL); !errors.Is(err, auth.ErrRenewalNotAllowed) {
		t.Fatalf("past its assumed lifetime: %v", err)
	}
	if c.sim.Issued() != 0 || len(store.tokens) != 0 {
		t.Errorf("%d tokens issued, %d stored", c.sim.Issued(), len(store.tokens))
	}
}

func readOnlyManager(t *testing.T, clk clock.Clock, store auth.TokenStore) *auth.Manager {
	t.Helper()
	m, err := auth.NewManager(auth.ManagerOptions{Clock: clk, Store: store, Plugins: auth.NewPlugins(New()), ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	m.SetSender(sender{})
	return m
}

func TestReauthRequired(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, time.November, 3, 9, 0, 0, 0, time.UTC))
	c := newCentral(t, clk)
	store := &memStore{tokens: map[string][]byte{}}
	var tokens []string
	m := manager(t, clk, store, &tokens)
	cfg := map[string]any{"clientId": pluginConfig["clientId"], "clientSecret": pluginConfig["clientSecret"], "refreshToken": pluginConfig["refreshToken"]}
	a, err := m.For(&config.Auth{Type: config.AuthPlugin, Plugin: &config.PluginAuth{Name: Name, Config: cfg}}, session(c.srv.URL, "unknown-refresh"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, a, c.srv.URL); !errors.Is(err, auth.ErrReauthRequired) {
		t.Fatalf("rejected refresh token: %v", err)
	}
	if _, reauth := a.(auth.PluginAuthenticator).Status(); !reauth {
		t.Error("Status() does not report the re-bootstrap")
	}
	issued := c.sim.Issued()
	if _, err := call(t, a, c.srv.URL); !errors.Is(err, auth.ErrReauthRequired) || c.sim.Issued() != issued {
		t.Errorf("the plugin called the API again: %v", err)
	}
	if info := m.Sessions(); len(info) != 1 || !info[0].Reauth || info[0].Account != "campus" {
		t.Errorf("Sessions() = %+v", info)
	}
	if !store.stored(t).Reauth {
		t.Error("the condition is not stored")
	}
}

func TestValidate(t *testing.T) {
	p := New()
	refs, err := p.Validate(pluginConfig)
	if err != nil || strings.Join(refs, ",") != "clientId,clientSecret,refreshToken,accessToken" {
		t.Fatalf("Validate() = %v, %v", refs, err)
	}
	for _, tc := range []struct {
		cfg  map[string]any
		want string
	}{
		{map[string]any{}, "clientId must be a secret reference"},
		{map[string]any{"clientId": "literal", "clientSecret": pluginConfig["clientSecret"], "refreshToken": pluginConfig["refreshToken"]}, "clientId must be a secret reference"},
		{map[string]any{"clientId": pluginConfig["clientId"], "clientSecret": pluginConfig["clientSecret"], "refreshToken": pluginConfig["refreshToken"], "nope": 1}, `unknown config field "nope"`},
		{map[string]any{"clientId": pluginConfig["clientId"], "clientSecret": pluginConfig["clientSecret"], "refreshToken": pluginConfig["refreshToken"], "tokenPath": "oauth"}, "tokenPath must be a path"},
		{map[string]any{"clientId": pluginConfig["clientId"], "clientSecret": pluginConfig["clientSecret"], "refreshToken": pluginConfig["refreshToken"], "refreshBefore": "3h"}, "refreshBefore must be a duration below 2h"},
	} {
		if _, err := p.Validate(tc.cfg); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("Validate(%v) = %v, want %q", tc.cfg, err, tc.want)
		}
	}
	if _, err := p.Simulator(auth.SimulatorEnv{}, map[string]any{}); err == nil {
		t.Error("Simulator() accepted no credentials")
	}
}
