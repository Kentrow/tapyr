package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kentrow/tapyr/internal/clock"
	"github.com/kentrow/tapyr/internal/config"
	"github.com/kentrow/tapyr/internal/mapping"
	"github.com/kentrow/tapyr/internal/secret"
)

var start = time.Date(2026, time.November, 2, 9, 0, 0, 0, time.UTC)

// directSender sends token requests without a budget engine and records
// their scopes and endpoints.
type directSender struct {
	mu    sync.Mutex
	calls []string
}

func (d *directSender) SendAuth(_ context.Context, scope Scope, endpoint string, req *http.Request) (*http.Response, error) {
	d.mu.Lock()
	d.calls = append(d.calls, scope.Account+"/"+scope.Target+" "+req.Method+" "+endpoint)
	d.mu.Unlock()
	return http.DefaultClient.Do(req)
}

type memStore struct {
	mu     sync.Mutex
	tokens map[string][]byte
	expiry map[string]time.Time
}

func newMemStore() *memStore {
	return &memStore{tokens: map[string][]byte{}, expiry: map[string]time.Time{}}
}

func (m *memStore) LoadToken(_ context.Context, key string) ([]byte, time.Time, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tokens[key]
	return t, m.expiry[key], ok, nil
}

func (m *memStore) SaveToken(_ context.Context, key string, sealed []byte, expiry time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tokens[key], m.expiry[key] = sealed, expiry
	return nil
}

func (m *memStore) DeleteToken(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.tokens, key)
	return nil
}

// tokenServer issues tok-1, tok-2... and records the form of every request.
type tokenServer struct {
	*httptest.Server
	issued atomic.Int64
	mu     sync.Mutex
	forms  []url.Values
	users  []string
	status int
}

func newTokenServer(t *testing.T, expiresIn string) *tokenServer {
	t.Helper()
	ts := &tokenServer{status: http.StatusOK}
	ts.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		user, pass, _ := r.BasicAuth()
		ts.mu.Lock()
		ts.forms = append(ts.forms, r.PostForm)
		ts.users = append(ts.users, user+":"+pass)
		status := ts.status
		ts.mu.Unlock()
		if status != http.StatusOK {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error": "invalid_client", "client_secret": "s3cr3t"}`))
			return
		}
		n := ts.issued.Add(1)
		_, _ = fmt.Fprintf(w, `{"access_token": "tok-%d", "token_type": "Bearer"%s}`, n, expiresIn)
	}))
	t.Cleanup(ts.Close)
	return ts
}

func secretsOf(m map[string]string) Secrets {
	out := Secrets{}
	for k, v := range m {
		out[k] = secret.New(v)
	}
	return out
}

func oauth2Auth(tokenURL, style string) *config.Auth {
	return &config.Auth{Type: config.AuthOAuth2ClientCredentials, OAuth2ClientCredentials: &config.OAuth2ClientCredentials{
		TokenURL: config.Template(tokenURL), ClientID: config.Value{Secret: "id"}, ClientSecret: config.Value{Secret: "secret"},
		Scopes: []string{"read", "metrics"}, Audience: "api", ExtraParams: map[string]string{"tenant": "t1"}, AuthStyle: style,
	}}
}

func authorization(t *testing.T, a Authenticator) string {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "http://api/x", http.NoBody)
	if err := a.Decorate(context.Background(), req); err != nil {
		t.Fatalf("Decorate() = %v", err)
	}
	return req.Header.Get("Authorization")
}

func newManager(t *testing.T, clk clock.Clock, store TokenStore, key string) (*Manager, *directSender) {
	t.Helper()
	m, err := NewManager(ManagerOptions{Clock: clk, Store: store, StateKey: key})
	if err != nil {
		t.Fatal(err)
	}
	s := &directSender{}
	m.SetSender(s)
	return m, s
}

func TestOAuth2ClientCredentials(t *testing.T) {
	ts := newTokenServer(t, `, "expires_in": 600`)
	clk := clock.NewFake(start)
	store := newMemStore()
	m, sender := newManager(t, clk, store, "state-key")
	session := Session{Scope: Scope{Provider: "p", Account: "acme", Credential: "c1"}, Secrets: secretsOf(map[string]string{"id": "client id", "secret": "s3cr3t"})}
	a, err := m.For(oauth2Auth(ts.URL+"/oauth/token", ""), session)
	if err != nil {
		t.Fatal(err)
	}
	// Jobs of the same account and credential share the session.
	b, err := m.For(oauth2Auth(ts.URL+"/oauth/token", ""), session)
	if err != nil || a != b {
		t.Fatalf("sessions not shared: %v", err)
	}

	// Concurrent requests share one token request.
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			if got := authorization(t, a); got != "Bearer tok-1" {
				t.Errorf("Authorization = %q", got)
			}
		})
	}
	wg.Wait()
	if n := ts.issued.Load(); n != 1 {
		t.Fatalf("%d token requests, want 1", n)
	}
	form := ts.forms[0]
	if form.Get("grant_type") != "client_credentials" || form.Get("scope") != "read metrics" || form.Get("audience") != "api" ||
		form.Get("tenant") != "t1" || form.Get("client_secret") != "" || ts.users[0] != "client+id:s3cr3t" {
		t.Errorf("token request: form %v, basic %q", form, ts.users[0])
	}
	if sender.calls[0] != "acme/ POST /oauth/token" {
		t.Errorf("auth request = %q", sender.calls[0])
	}

	// Renewed 30s before expiry.
	clk.Advance(9*time.Minute + 29*time.Second)
	if got := authorization(t, a); got != "Bearer tok-1" {
		t.Errorf("before renewal: %q", got)
	}
	clk.Advance(time.Second)
	if got := authorization(t, a); got != "Bearer tok-2" {
		t.Errorf("after renewal: %q", got)
	}
	// A rejected token is dropped and replaced once.
	if retry, err := a.OnAuthFailure(context.Background(), &http.Response{StatusCode: http.StatusUnauthorized}); !retry || err != nil {
		t.Fatalf("OnAuthFailure() = %v, %v", retry, err)
	}
	if got := authorization(t, a); got != "Bearer tok-3" {
		t.Errorf("after a rejection: %q", got)
	}

	// The token survives a restart, sealed with the state key.
	for key, sealed := range store.tokens {
		if strings.Contains(string(sealed), "tok-3") || !strings.Contains(key, "c1") {
			t.Errorf("stored token %q = %q, want sealed", key, sealed)
		}
	}
	m2, _ := newManager(t, clk, store, "state-key")
	a2, _ := m2.For(oauth2Auth(ts.URL+"/oauth/token", ""), session)
	if got := authorization(t, a2); got != "Bearer tok-3" || ts.issued.Load() != 3 {
		t.Errorf("after a restart: %q, %d token requests", got, ts.issued.Load())
	}
	// Another key cannot open it, and no key ignores sealed tokens.
	for _, key := range []string{"other-key", ""} {
		m3, _ := newManager(t, clk, store, key)
		a3, _ := m3.For(oauth2Auth(ts.URL+"/oauth/token", ""), session)
		authorization(t, a3)
	}
	if n := ts.issued.Load(); n != 5 {
		t.Errorf("%d token requests, want 5", n)
	}
	// The last token was stored unencrypted, which a keyed manager ignores.
	m4, _ := newManager(t, clk, store, "state-key")
	a4, _ := m4.For(oauth2Auth(ts.URL+"/oauth/token", ""), session)
	if got := authorization(t, a4); got != "Bearer tok-6" {
		t.Errorf("plain token reused with a key: %q", got)
	}
}

// TestReadOnlyManager checks that a read-only Manager (tapyr record next to
// tapyr run) never writes or deletes stored tokens, and does not use them:
// their sessions belong to tapyr run, which a logout would end.
func TestReadOnlyManager(t *testing.T) {
	ts := newTokenServer(t, `, "expires_in": 600`)
	clk := clock.NewFake(start)
	store := newMemStore()
	session := Session{Scope: Scope{Provider: "p", Account: "acme", Credential: "c1"}, Secrets: secretsOf(map[string]string{"id": "client id", "secret": "s3cr3t"})}
	m, _ := newManager(t, clk, store, "state-key")
	a, _ := m.For(oauth2Auth(ts.URL+"/oauth/token", ""), session)
	authorization(t, a)
	snapshot := maps.Clone(store.tokens)

	ro, err := NewManager(ManagerOptions{Clock: clk, Store: store, StateKey: "state-key", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	ro.SetSender(&directSender{})
	b, err := ro.For(oauth2Auth(ts.URL+"/oauth/token", ""), session)
	if err != nil {
		t.Fatal(err)
	}
	if got := authorization(t, b); got != "Bearer tok-2" {
		t.Fatalf("read-only token: %q", got)
	}
	// A renewed token is kept in memory, a rejected one is not deleted.
	clk.Advance(10 * time.Minute)
	if got := authorization(t, b); got != "Bearer tok-3" {
		t.Errorf("after expiry: %q", got)
	}
	if _, err := b.OnAuthFailure(context.Background(), &http.Response{StatusCode: http.StatusUnauthorized}); err != nil {
		t.Fatal(err)
	}
	if !maps.EqualFunc(store.tokens, snapshot, bytes.Equal) {
		t.Error("the read-only manager changed the store")
	}
}

func TestOAuth2BodyStyleAndErrors(t *testing.T) {
	ts := newTokenServer(t, "")
	clk := clock.NewFake(start)
	m, _ := newManager(t, clk, nil, "")
	session := Session{Scope: Scope{Account: "acme"}, Secrets: secretsOf(map[string]string{"id": "cid", "secret": "s3cr3t"})}
	a, err := m.For(oauth2Auth(ts.URL+"/token", "body"), session)
	if err != nil {
		t.Fatal(err)
	}
	if got := authorization(t, a); got != "Bearer tok-1" || ts.forms[0].Get("client_secret") != "s3cr3t" || ts.users[0] != ":" {
		t.Errorf("body style: %q, form %v", got, ts.forms[0])
	}
	// Without expires_in, the token lives DefaultTokenLifetime.
	clk.Advance(DefaultTokenLifetime - DefaultRefreshBefore - time.Second)
	if got := authorization(t, a); got != "Bearer tok-1" {
		t.Errorf("within the default lifetime: %q", got)
	}

	ts.mu.Lock()
	ts.status = http.StatusUnauthorized
	ts.mu.Unlock()
	if _, err := a.OnAuthFailure(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "http://api/x", http.NoBody)
	err = a.Decorate(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "token endpoint /token answered 401") || strings.Contains(err.Error(), "s3cr3t") {
		t.Errorf("Decorate() error = %v", err)
	}

	for name, tc := range map[string]struct {
		auth    *config.Auth
		secrets Secrets
		want    string
	}{
		"relative URL":   {oauth2Auth("/token", ""), session.Secrets, "is not an absolute http or https URL"},
		"missing secret": {oauth2Auth(ts.URL, ""), Secrets{}, `auth clientId: secret "id" is not provided`},
	} {
		if _, err := m.For(tc.auth, Session{Secrets: tc.secrets}); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error = %v, want %q", name, err, tc.want)
		}
	}
	noSender, _ := NewManager(ManagerOptions{})
	a, _ = noSender.For(oauth2Auth(ts.URL, ""), session)
	if err := a.Decorate(context.Background(), req); err == nil || !strings.Contains(err.Error(), "not available in this command") {
		t.Errorf("without a sender: %v", err)
	}
}

func TestOAuth2InvalidResponses(t *testing.T) {
	for name, body := range map[string]string{
		"no token":           `{"token_type": "Bearer"}`,
		"invalid expires_in": `{"access_token": "x", "expires_in": "soon"}`,
		"not JSON":           `<html>`,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) }))
		m, _ := newManager(t, clock.NewFake(start), nil, "")
		a, _ := m.For(oauth2Auth(srv.URL, ""), Session{Secrets: secretsOf(map[string]string{"id": "i", "secret": "s"})})
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "http://api/x", http.NoBody)
		if err := a.Decorate(context.Background(), req); err == nil {
			t.Errorf("%s: Decorate() succeeded", name)
		}
		srv.Close()
	}
}

func loginPrograms(t *testing.T, token, expiresIn string, body map[string]string) *LoginPrograms {
	t.Helper()
	env, err := mapping.NewEnv(0)
	if err != nil {
		t.Fatal(err)
	}
	lp := &LoginPrograms{Body: map[string]*mapping.Program{}}
	if lp.Token, err = env.Compile(mapping.ContextLogin, token, mapping.OutputString); err != nil {
		t.Fatal(err)
	}
	if expiresIn != "" {
		if lp.ExpiresIn, err = env.Compile(mapping.ContextLogin, expiresIn, mapping.OutputDuration); err != nil {
			t.Fatal(err)
		}
	}
	for k, src := range body {
		if lp.Body[k], err = env.Compile(mapping.ContextRequest, src, mapping.OutputString); err != nil {
			t.Fatal(err)
		}
	}
	return lp
}

func TestSessionLogin(t *testing.T) {
	var logins atomic.Int64
	var mu sync.Mutex
	var bodies []map[string]string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		if r.URL.Path != "/api/v1/login" || r.Method != http.MethodPost || r.Header.Get("X-Client") != "tapyr" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		n := logins.Add(1)
		_, _ = fmt.Fprintf(w, `{"data": {"token": "sess-%d", "expires": 120}}`, n)
	}))
	defer api.Close()
	base, _ := url.Parse(api.URL + "/api/v1")
	clk := clock.NewFake(start)
	m, sender := newManager(t, clk, nil, "")
	login := func(apply config.TokenApply, perTarget bool) *config.Auth {
		return &config.Auth{Type: config.AuthSessionLogin, SessionLogin: &config.SessionLogin{
			Request: config.LoginRequest{Path: "/login", Headers: map[string]string{"X-Client": "tapyr"}, Body: map[string]config.Value{
				"username": {Secret: "user"}, "password": {Secret: "pass"}, "device": {Expr: "target.name + '-' + vars.site"}, "mode": {Literal: "api"},
			}},
			Token: "response.body.data.token", Apply: apply, PerTarget: perTarget, RefreshBefore: config.Duration(10 * time.Second),
		}}
	}
	lp := loginPrograms(t, "response.body.data.token", "duration(string(response.body.data.expires) + 's')",
		map[string]string{"device": "target.name + '-' + vars.site"})
	session := func(target string) Session {
		return Session{
			Scope: Scope{Provider: "routers", Account: "fleet", Target: target, Credential: "c"}, BaseURL: base,
			Secrets: secretsOf(map[string]string{"user": "admin", "pass": "s3cr3t"}), Vars: map[string]string{"site": "north"},
			Target: map[string]any{"name": target}, Login: lp,
		}
	}

	// Header with a format, one session per target.
	header := config.TokenApply{Header: "Authorization", Format: "Bearer {token}"}
	r1, err := m.For(login(header, true), session("r1"))
	if err != nil {
		t.Fatal(err)
	}
	r2, _ := m.For(login(header, true), session("r2"))
	if a, b := authorization(t, r1), authorization(t, r2); a != "Bearer sess-1" || b != "Bearer sess-2" {
		t.Errorf("per-target sessions: %q %q", a, b)
	}
	if bodies[0]["username"] != "admin" || bodies[0]["password"] != "s3cr3t" || bodies[0]["device"] != "r1-north" || bodies[0]["mode"] != "api" {
		t.Errorf("login body = %v", bodies[0])
	}
	if sender.calls[0] != "fleet/r1 POST /login" {
		t.Errorf("auth request = %q", sender.calls[0])
	}
	// expiresIn 120s, refreshBefore 10s.
	clk.Advance(110 * time.Second)
	if got := authorization(t, r1); got != "Bearer sess-3" {
		t.Errorf("after renewal: %q", got)
	}

	// Cookie, one session per account.
	cookie := config.TokenApply{Cookie: "sid"}
	c1, _ := m.For(login(cookie, false), session("r1"))
	c2, _ := m.For(login(cookie, false), session("r2"))
	if c1 != c2 {
		t.Error("sessions without perTarget not shared")
	}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "http://api/x", http.NoBody)
	if err := c2.Decorate(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if ck, err := req.Cookie("sid"); err != nil || ck.Value != "sess-4" {
		t.Errorf("cookie = %v, %v", ck, err)
	}

	// Failures name the login path, never the response or the secrets.
	bad := login(header, false)
	bad.SessionLogin.Request.Path = "/nope"
	a, _ := m.For(bad, session("r3"))
	err = a.Decorate(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "login /nope answered 404") || strings.Contains(err.Error(), "s3cr3t") {
		t.Errorf("failed login error = %v", err)
	}
	if _, err := m.For(login(header, false), Session{BaseURL: base}); err == nil {
		t.Error("login without compiled programs accepted")
	}
	noSecret := session("r4")
	noSecret.Secrets = Secrets{}
	if _, err := m.For(login(header, true), noSecret); err == nil || !strings.Contains(err.Error(), `secret "pass"`) && !strings.Contains(err.Error(), `secret "user"`) {
		t.Errorf("missing secret error = %v", err)
	}
}

func TestLoginTokenExpressions(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"token": "", "ttl": 0}`)
	}))
	defer api.Close()
	base, _ := url.Parse(api.URL)
	auth := &config.Auth{Type: config.AuthSessionLogin, SessionLogin: &config.SessionLogin{
		Request: config.LoginRequest{Path: "/login"}, Token: "x", Apply: config.TokenApply{Header: "X-Token"},
	}}
	for name, lp := range map[string]*LoginPrograms{
		"empty token":    loginPrograms(t, "response.body.token", "", nil),
		"zero lifetime":  loginPrograms(t, "'t'", "duration(string(response.body.ttl) + 's')", nil),
		"missing field":  loginPrograms(t, "response.body.missing", "", nil),
		"failed expires": loginPrograms(t, "'t'", "duration(response.body.missing)", nil),
	} {
		m, _ := newManager(t, clock.NewFake(start), nil, "")
		a, err := m.For(auth, Session{Scope: Scope{Account: name}, BaseURL: base, Login: lp})
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "http://api/x", http.NoBody)
		if err := a.Decorate(context.Background(), req); err == nil {
			t.Errorf("%s: Decorate() succeeded", name)
		}
	}
}

type tokenObserver struct {
	mu      sync.Mutex
	results []string
}

func (o *tokenObserver) TokenRequest(provider, account, result string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.results = append(o.results, provider+"/"+account+" "+result)
}

func TestObserverAndSessions(t *testing.T) {
	ts := newTokenServer(t, `, "expires_in": 600`)
	clk := clock.NewFake(start)
	obs := &tokenObserver{}
	var tokens []string
	m, err := NewManager(ManagerOptions{Clock: clk, Observer: obs, OnToken: func(tok string) { tokens = append(tokens, tok) }})
	if err != nil {
		t.Fatal(err)
	}
	m.SetSender(&directSender{})
	session := Session{Scope: Scope{Provider: "p", Account: "acme", Target: "t1"}, Secrets: secretsOf(map[string]string{"id": "i", "secret": "s"})}
	a, err := m.For(oauth2Auth(ts.URL+"/oauth/token", ""), session)
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Sessions(); len(got) != 1 || !got[0].Expiry.IsZero() {
		t.Errorf("sessions before the first token = %+v", got)
	}
	authorization(t, a)
	if got := m.Sessions(); len(got) != 1 || got[0].Provider != "p" || got[0].Account != "acme" || got[0].Target != "t1" ||
		!got[0].Expiry.Equal(start.Add(10*time.Minute)) {
		t.Errorf("sessions = %+v", got)
	}
	// A failed request is observed too.
	bad, _ := m.For(oauth2Auth("http://127.0.0.1:1/oauth/token", ""), session)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "http://api/x", http.NoBody)
	if err := bad.Decorate(context.Background(), req); err == nil {
		t.Fatal("Decorate with an unreachable token endpoint succeeded")
	}
	obs.mu.Lock()
	defer obs.mu.Unlock()
	if !cmp.Equal(obs.results, []string{"p/acme success", "p/acme failed"}) || !cmp.Equal(tokens, []string{"tok-1"}) {
		t.Errorf("observed %v, tokens %v", obs.results, tokens)
	}
}

// TestRejectionBackoff sends no login while the credentials were just
// rejected: 1 minute after the first rejection, 2 after the second, and the
// backoff ends with a successful login.
func TestRejectionBackoff(t *testing.T) {
	var logins atomic.Int64
	var accept atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		logins.Add(1)
		if !accept.Load() {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, `{"token": "ok"}`)
	}))
	defer srv.Close()
	clk := clock.NewFake(start)
	m, _ := newManager(t, clk, nil, "")
	base, _ := url.Parse(srv.URL)
	a, err := m.For(&config.Auth{Type: config.AuthSessionLogin, SessionLogin: &config.SessionLogin{
		Request: config.LoginRequest{Path: "/login"}, Token: "response.body.token", Apply: config.TokenApply{Header: "Authorization"},
	}}, Session{Scope: Scope{Provider: "p", Account: "acme"}, BaseURL: base, Login: loginPrograms(t, "response.body.token", "", nil)})
	if err != nil {
		t.Fatal(err)
	}
	decorate := func() error {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/x", http.NoBody)
		return a.Decorate(context.Background(), req)
	}
	for _, step := range []struct {
		advance time.Duration
		logins  int64
	}{
		{0, 1},                // rejected: wait 1m
		{30 * time.Second, 1}, // waiting
		{30 * time.Second, 2}, // rejected again: wait 2m
		{time.Minute, 2},      // waiting
		{time.Minute, 3},      // rejected: wait 4m
	} {
		clk.Advance(step.advance)
		if err := decorate(); err == nil || !strings.Contains(err.Error(), "the credentials were rejected") {
			t.Fatalf("Decorate() = %v", err)
		}
		if n := logins.Load(); n != step.logins {
			t.Fatalf("after %v: %d logins, want %d", clk.Since(start), n, step.logins)
		}
	}
	accept.Store(true)
	clk.Advance(4 * time.Minute)
	if err := decorate(); err != nil || logins.Load() != 4 {
		t.Fatalf("after the backoff: %v, %d logins", err, logins.Load())
	}
}

// TestSessionLogout: Close logs out every open session with its token,
// through the sender (the budget engine); a failed logout is not an error
// (the session expires on its own).
func TestSessionLogout(t *testing.T) {
	var mu sync.Mutex
	var logouts []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/login":
			_, _ = fmt.Fprint(w, `{"token": "sess-1"}`)
		case "/api/logout":
			mu.Lock()
			logouts = append(logouts, r.Method+" "+r.Header.Get("Authorization"))
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case "/api/broken-logout":
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer api.Close()
	base, _ := url.Parse(api.URL + "/api")
	m, sender := newManager(t, clock.NewFake(start), nil, "")
	login := func(logout string) *config.Auth {
		return &config.Auth{Type: config.AuthSessionLogin, SessionLogin: &config.SessionLogin{
			Request: config.LoginRequest{Path: "/login", Body: map[string]config.Value{"username": {Literal: "u"}}},
			Token:   "response.body.token", Apply: config.TokenApply{Header: "Authorization", Format: "Bearer {token}"},
			Logout: &config.LogoutRequest{Path: logout, Method: http.MethodDelete},
		}}
	}
	session := func(account string) Session {
		return Session{
			Scope: Scope{Provider: "routers", Account: account, Credential: account}, BaseURL: base,
			Login: loginPrograms(t, "response.body.token", "", nil),
		}
	}
	a, err := m.For(login("/logout"), session("fleet"))
	if err != nil {
		t.Fatal(err)
	}
	if got := authorization(t, a); got != "Bearer sess-1" {
		t.Fatalf("authorization = %q", got)
	}
	b, err := m.For(login("/broken-logout"), session("other"))
	if err != nil {
		t.Fatal(err)
	}
	authorization(t, b)
	m.Close(context.Background())
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(logouts, "|") != "DELETE Bearer sess-1" {
		t.Errorf("logouts = %v", logouts)
	}
	if !slices.Contains(sender.calls, "fleet/ DELETE /logout") || !slices.Contains(sender.calls, "other/ DELETE /broken-logout") {
		t.Errorf("logout requests did not go through the sender: %v", sender.calls)
	}
}
