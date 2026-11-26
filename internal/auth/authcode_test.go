package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kentrow/tapyr/internal/clock"
	"github.com/kentrow/tapyr/internal/config"
)

// refreshServer is a token endpoint for the refresh_token grant. It issues
// access-1, access-2... and, when rotate is set, refresh-1, refresh-2...,
// spending the refresh token it was given.
type refreshServer struct {
	*httptest.Server
	mu        sync.Mutex
	rotate    bool
	expiresIn string // the expires_in member, with its comma, or empty
	valid     map[string]bool
	forms     []url.Values
	users     []string
	n         int
	// answer, when set, replaces the response: status and body.
	answerStatus int
	answerBody   string
	// onRequest runs before the response, for tests that look at the store.
	onRequest func()
}

func newRefreshServer(t *testing.T, bootstrap string, rotate bool) *refreshServer {
	t.Helper()
	rs := &refreshServer{rotate: rotate, expiresIn: `, "expires_in": 3600`, valid: map[string]bool{bootstrap: true}}
	rs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		user, pass, _ := r.BasicAuth()
		rs.mu.Lock()
		defer rs.mu.Unlock()
		rs.forms = append(rs.forms, r.PostForm)
		rs.users = append(rs.users, user+":"+pass)
		if rs.onRequest != nil {
			rs.onRequest()
		}
		if rs.answerStatus != 0 {
			w.WriteHeader(rs.answerStatus)
			_, _ = w.Write([]byte(rs.answerBody))
			return
		}
		rt := r.PostForm.Get("refresh_token")
		if r.PostForm.Get("grant_type") != "refresh_token" || !rs.valid[rt] {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error": "invalid_grant"}`))
			return
		}
		rs.n++
		refresh := ""
		if rs.rotate {
			delete(rs.valid, rt)
			next := fmt.Sprintf("refresh-%d", rs.n)
			rs.valid[next] = true
			refresh = fmt.Sprintf(`, "refresh_token": %q`, next)
		}
		_, _ = fmt.Fprintf(w, `{"access_token": "access-%d", "token_type": "Bearer"%s%s}`, rs.n, rs.expiresIn, refresh)
	}))
	t.Cleanup(rs.Close)
	return rs
}

func (rs *refreshServer) requests() int {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return len(rs.forms)
}

func (rs *refreshServer) answer(status int, body string) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.answerStatus, rs.answerBody = status, body
}

func authCodeAuth(tokenURL, style string) *config.Auth {
	sec := config.Value{Secret: "clientSecret"}
	return &config.Auth{Type: config.AuthOAuth2AuthorizationCode, OAuth2AuthorizationCode: &config.OAuth2AuthorizationCode{
		AuthorizeURL: "https://auth.example.invalid/authorize", TokenURL: config.Template(tokenURL),
		ClientID: config.Value{Secret: "clientId"}, ClientSecret: &sec, RefreshToken: config.Value{Secret: "refreshToken"},
		ScopeSeparator: " ", PKCE: "S256", AuthStyle: style, RefreshBefore: config.Duration(5 * time.Minute),
		TokenParams: map[string]string{"tenant": "t1"},
	}}
}

func authCodeSession(account, refresh string) Session {
	return Session{
		Scope:   Scope{Provider: "athletes", Account: account, Credential: "c-" + account},
		Secrets: secretsOf(map[string]string{"clientId": "id", "clientSecret": "s3cr3t", "refreshToken": refresh}),
	}
}

func authCodeFor(t *testing.T, m *Manager, a *config.Auth, s Session) Authenticator {
	t.Helper()
	au, err := m.For(a, s)
	if err != nil {
		t.Fatalf("For() = %v", err)
	}
	return au
}

func decorate(a Authenticator) (string, error) {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "http://api/x", http.NoBody)
	err := a.Decorate(context.Background(), req)
	return req.Header.Get("Authorization"), err
}

// storedPair opens the token pair stored for an account.
func storedPair(t *testing.T, m *Manager, account string) authCodeState {
	t.Helper()
	v, _, ok := m.load(context.Background(), "authcode|athletes|"+account)
	if !ok {
		t.Fatalf("no stored pair for %s", account)
	}
	var st authCodeState
	if err := json.Unmarshal([]byte(v.Reveal()), &st); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestAuthCodeBootstrap(t *testing.T) {
	for _, style := range []string{"header", "body"} {
		t.Run(style, func(t *testing.T) {
			rs := newRefreshServer(t, "boot", false)
			m, sender := newManager(t, clock.NewFake(start), newMemStore(), "k")
			a := authCodeFor(t, m, authCodeAuth(rs.URL+"/oauth/token", style), authCodeSession("home", "boot"))
			if got := authorization(t, a); got != "Bearer access-1" {
				t.Fatalf("Authorization = %q", got)
			}
			form := rs.forms[0]
			if form.Get("grant_type") != "refresh_token" || form.Get("refresh_token") != "boot" || form.Get("tenant") != "t1" {
				t.Errorf("form = %v", form)
			}
			switch style {
			case "header":
				if rs.users[0] != "id:s3cr3t" || form.Has("client_secret") {
					t.Errorf("client auth: user %q, form %v", rs.users[0], form)
				}
			case "body":
				if rs.users[0] != ":" || form.Get("client_id") != "id" || form.Get("client_secret") != "s3cr3t" {
					t.Errorf("client auth: user %q, form %v", rs.users[0], form)
				}
			}
			if len(sender.calls) != 1 || sender.calls[0] != "home/ POST /oauth/token" {
				t.Errorf("token requests through the sender: %v", sender.calls)
			}
		})
	}
}

func TestAuthCodePublicClient(t *testing.T) {
	// Without a client secret, the client id goes in the body (RFC 6749,
	// section 2.3.1 is for confidential clients).
	rs := newRefreshServer(t, "boot", false)
	m, _ := newManager(t, clock.NewFake(start), newMemStore(), "k")
	// The block names a clientSecret the account does not provide.
	s := authCodeSession("home", "boot")
	delete(s.Secrets, "clientSecret")
	authorization(t, authCodeFor(t, m, authCodeAuth(rs.URL+"/token", "header"), s))
	if rs.users[0] != ":" || rs.forms[0].Get("client_id") != "id" || rs.forms[0].Has("client_secret") {
		t.Errorf("public client: user %q, form %v", rs.users[0], rs.forms[0])
	}
}

func TestAuthCodeRotationPersistedBeforeUse(t *testing.T) {
	rs := newRefreshServer(t, "boot", true)
	clk := clock.NewFake(start)
	m, _ := newManager(t, clk, newMemStore(), "k")
	a := authCodeFor(t, m, authCodeAuth(rs.URL+"/token", "header"), authCodeSession("home", "boot"))
	authorization(t, a)
	if st := storedPair(t, m, "home"); st.Refresh != "refresh-1" || st.Access != "access-1" {
		t.Fatalf("stored after the first refresh: %+v", st)
	}
	// The next refresh, 5 minutes before the expiry, sends refresh-1.
	clk.Advance(56 * time.Minute)
	if got := authorization(t, a); got != "Bearer access-2" {
		t.Fatalf("Authorization = %q", got)
	}
	if rs.forms[1].Get("refresh_token") != "refresh-1" {
		t.Errorf("second refresh sent %q", rs.forms[1].Get("refresh_token"))
	}
	if st := storedPair(t, m, "home"); st.Refresh != "refresh-2" {
		t.Errorf("stored refresh token = %q, want refresh-2", st.Refresh)
	}
}

func TestAuthCodeNoRotationKeepsRefresh(t *testing.T) {
	rs := newRefreshServer(t, "boot", false)
	clk := clock.NewFake(start)
	m, _ := newManager(t, clk, newMemStore(), "k")
	a := authCodeFor(t, m, authCodeAuth(rs.URL+"/token", "header"), authCodeSession("home", "boot"))
	authorization(t, a)
	clk.Advance(time.Hour)
	authorization(t, a)
	if got := rs.forms[1].Get("refresh_token"); got != "boot" {
		t.Errorf("second refresh sent %q, want the bootstrap token", got)
	}
	if st := storedPair(t, m, "home"); st.Refresh != "boot" {
		t.Errorf("stored refresh token = %q", st.Refresh)
	}
}

func TestAuthCodeRestartUsesStoredPair(t *testing.T) {
	rs := newRefreshServer(t, "boot", true)
	clk := clock.NewFake(start)
	store := newMemStore()
	m1, _ := newManager(t, clk, store, "k")
	authorization(t, authCodeFor(t, m1, authCodeAuth(rs.URL+"/token", "header"), authCodeSession("home", "boot")))

	// A restart 10 minutes later: same store, same (now spent) bootstrap.
	clk.Advance(10 * time.Minute)
	m2, _ := newManager(t, clk, store, "k")
	a := authCodeFor(t, m2, authCodeAuth(rs.URL+"/token", "header"), authCodeSession("home", "boot"))
	if got := authorization(t, a); got != "Bearer access-1" || rs.requests() != 1 {
		t.Fatalf("after restart: %q, %d token requests", got, rs.requests())
	}
	clk.Advance(50 * time.Minute)
	if got := authorization(t, a); got != "Bearer access-2" || rs.forms[1].Get("refresh_token") != "refresh-1" {
		t.Errorf("refresh after restart: %q with %v", got, rs.forms[1])
	}
}

func TestAuthCodeNewBootstrapReplacesPair(t *testing.T) {
	// A new tapyr auth login (a new secret, then a reload) replaces the
	// stored pair, even a valid one.
	rs := newRefreshServer(t, "boot-1", true)
	rs.valid["boot-2"] = true
	clk := clock.NewFake(start)
	m, _ := newManager(t, clk, newMemStore(), "k")
	authorization(t, authCodeFor(t, m, authCodeAuth(rs.URL+"/token", "header"), authCodeSession("home", "boot-1")))
	a := authCodeFor(t, m, authCodeAuth(rs.URL+"/token", "header"), authCodeSession("home", "boot-2"))
	if got := authorization(t, a); got != "Bearer access-2" {
		t.Fatalf("Authorization = %q", got)
	}
	if got := rs.forms[1].Get("refresh_token"); got != "boot-2" {
		t.Errorf("refresh sent %q, want the new bootstrap", got)
	}
}

func TestAuthCodeInvalidGrantLatches(t *testing.T) {
	rs := newRefreshServer(t, "boot", true)
	clk := clock.NewFake(start)
	store := newMemStore()
	m, _ := newManager(t, clk, store, "k")
	a := authCodeFor(t, m, authCodeAuth(rs.URL+"/token", "header"), authCodeSession("home", "spent"))
	if _, err := decorate(a); !errors.Is(err, ErrReauthRequired) {
		t.Fatalf("Decorate() = %v, want ErrReauthRequired", err)
	}
	clk.Advance(2 * time.Hour)
	if _, err := decorate(a); !errors.Is(err, ErrReauthRequired) || rs.requests() != 1 {
		t.Fatalf("second Decorate() = %v after %d token requests", err, rs.requests())
	}
	if retry, _ := a.OnAuthFailure(context.Background(), nil); retry {
		t.Error("OnAuthFailure asks for a retry with spent credentials")
	}
	if s := m.Sessions(); len(s) != 1 || !s[0].Reauth || s[0].Account != "home" {
		t.Errorf("Sessions() = %+v", s)
	}
	// It survives a restart while the secret is unchanged.
	m2, _ := newManager(t, clk, store, "k")
	a2 := authCodeFor(t, m2, authCodeAuth(rs.URL+"/token", "header"), authCodeSession("home", "spent"))
	if _, err := decorate(a2); !errors.Is(err, ErrReauthRequired) || rs.requests() != 1 {
		t.Errorf("after restart: %v, %d token requests", err, rs.requests())
	}
}

func TestAuthCodeInvalidClientDoesNotLatch(t *testing.T) {
	rs := newRefreshServer(t, "boot", true)
	rs.answer(http.StatusUnauthorized, `{"error": "invalid_client"}`)
	clk := clock.NewFake(start)
	m, _ := newManager(t, clk, newMemStore(), "k")
	a := authCodeFor(t, m, authCodeAuth(rs.URL+"/token", "header"), authCodeSession("home", "boot"))
	_, err := decorate(a)
	if err == nil || errors.Is(err, ErrReauthRequired) || !strings.Contains(err.Error(), "the credentials were rejected") {
		t.Fatalf("Decorate() = %v", err)
	}
	if s := m.Sessions(); s[0].Reauth {
		t.Error("invalid_client latched the re-bootstrap condition")
	}
	// The rejection backoff holds the next request, then the fixed client
	// works with the same refresh token.
	if _, err := decorate(a); err == nil || rs.requests() != 1 {
		t.Fatalf("within the backoff: %v, %d requests", err, rs.requests())
	}
	rs.answer(0, "")
	clk.Advance(time.Minute)
	if got, err := decorate(a); err != nil || got != "Bearer access-1" {
		t.Errorf("after the backoff: %q, %v", got, err)
	}
}

func TestAuthCodeTokenErrors(t *testing.T) {
	tests := []struct {
		name, body string
		status     int
		want       string
	}{
		{name: "400 without JSON", status: http.StatusBadRequest, body: "<html>bad</html>", want: "the credentials were rejected"},
		{name: "400 other error", status: http.StatusBadRequest, body: `{"error": "invalid_scope"}`, want: "the credentials were rejected"},
		{name: "200 without access_token", status: http.StatusOK, body: `{"token_type": "Bearer"}`, want: "no access_token in the response"},
		{name: "500", status: http.StatusInternalServerError, body: `{"error": "invalid_grant"}`, want: "answered 500"},
		{name: "bad expires_in", status: http.StatusOK, body: `{"access_token": "a", "expires_in": "soon"}`, want: "invalid expires_in"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rs := newRefreshServer(t, "boot", false)
			rs.answer(tt.status, tt.body)
			m, _ := newManager(t, clock.NewFake(start), newMemStore(), "k")
			_, err := decorate(authCodeFor(t, m, authCodeAuth(rs.URL+"/token", "header"), authCodeSession("home", "boot")))
			if err == nil || errors.Is(err, ErrReauthRequired) || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Decorate() = %v, want %q", err, tt.want)
			}
			if strings.Contains(fmt.Sprint(err), "boot") || strings.Contains(fmt.Sprint(err), "s3cr3t") {
				t.Errorf("the error carries a credential: %v", err)
			}
		})
	}
}

func TestAuthCodeNoExpiry(t *testing.T) {
	// An access token without expires_in is used until it is rejected.
	rs := newRefreshServer(t, "boot", false)
	rs.expiresIn = ""
	clk := clock.NewFake(start)
	m, _ := newManager(t, clk, newMemStore(), "k")
	a := authCodeFor(t, m, authCodeAuth(rs.URL+"/token", "header"), authCodeSession("home", "boot"))
	authorization(t, a)
	clk.Advance(72 * time.Hour)
	if got := authorization(t, a); got != "Bearer access-1" || rs.requests() != 1 {
		t.Fatalf("after 72h: %q, %d requests", got, rs.requests())
	}
	if retry, err := a.OnAuthFailure(context.Background(), nil); !retry || err != nil {
		t.Fatalf("OnAuthFailure() = %v, %v", retry, err)
	}
	if got := authorization(t, a); got != "Bearer access-2" {
		t.Errorf("after a rejection: %q", got)
	}
}

// failingStore refuses to save while fail is set.
type failingStore struct {
	*memStore
	mu   sync.Mutex
	fail bool
}

func (f *failingStore) SaveToken(ctx context.Context, key string, sealed []byte, expiry time.Time) error {
	f.mu.Lock()
	fail := f.fail
	f.mu.Unlock()
	if fail {
		return errors.New("disk full")
	}
	return f.memStore.SaveToken(ctx, key, sealed, expiry)
}

func TestAuthCodeSaveFailureBlocksUse(t *testing.T) {
	rs := newRefreshServer(t, "boot", true)
	store := &failingStore{memStore: newMemStore(), fail: true}
	m, _ := newManager(t, clock.NewFake(start), store, "k")
	a := authCodeFor(t, m, authCodeAuth(rs.URL+"/token", "header"), authCodeSession("home", "boot"))
	got, err := decorate(a)
	if err == nil || got != "" || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("Decorate() = %q, %v", got, err)
	}
	// The rotated pair is kept in memory and saved before its first use:
	// no second refresh, which the spent bootstrap would fail.
	store.mu.Lock()
	store.fail = false
	store.mu.Unlock()
	if got, err := decorate(a); err != nil || got != "Bearer access-1" || rs.requests() != 1 {
		t.Fatalf("after the store recovers: %q, %v, %d requests", got, err, rs.requests())
	}
	if st := storedPair(t, m, "home"); st.Refresh != "refresh-1" {
		t.Errorf("stored refresh token = %q", st.Refresh)
	}
}

func TestAuthCodeReadOnlyNeverRefreshes(t *testing.T) {
	rs := newRefreshServer(t, "boot", true)
	clk := clock.NewFake(start)
	store := newMemStore()
	m, _ := newManager(t, clk, store, "k")
	authorization(t, authCodeFor(t, m, authCodeAuth(rs.URL+"/token", "header"), authCodeSession("home", "boot")))

	ro, err := NewManager(ManagerOptions{Clock: clk, Store: store, StateKey: "k", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	ro.SetSender(&directSender{})
	a := authCodeFor(t, ro, authCodeAuth(rs.URL+"/token", "header"), authCodeSession("home", "boot"))
	if got := authorization(t, a); got != "Bearer access-1" {
		t.Fatalf("read-only: %q", got)
	}
	clk.Advance(58 * time.Minute) // inside refreshBefore, not expired: still used
	if got := authorization(t, a); got != "Bearer access-1" {
		t.Fatalf("read-only near the expiry: %q", got)
	}
	clk.Advance(3 * time.Minute)
	if _, err := decorate(a); !errors.Is(err, ErrRenewalNotAllowed) || rs.requests() != 1 {
		t.Errorf("read-only after the expiry: %v, %d requests", err, rs.requests())
	}
}

func TestAuthCodeSingleRefresh(t *testing.T) {
	rs := newRefreshServer(t, "boot", true)
	m, _ := newManager(t, clock.NewFake(start), newMemStore(), "k")
	a := authCodeFor(t, m, authCodeAuth(rs.URL+"/token", "header"), authCodeSession("home", "boot"))
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if _, err := decorate(a); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if rs.requests() != 1 {
		t.Errorf("%d token requests, want 1", rs.requests())
	}
}

func TestAuthCodeAccountsAreSeparate(t *testing.T) {
	rs := newRefreshServer(t, "boot-a", true)
	rs.valid["boot-b"] = true
	m, _ := newManager(t, clock.NewFake(start), newMemStore(), "k")
	cfg := authCodeAuth(rs.URL+"/token", "header")
	a := authCodeFor(t, m, cfg, authCodeSession("alice", "boot-a"))
	b := authCodeFor(t, m, cfg, authCodeSession("bob", "boot-b"))
	if authorization(t, a) == authorization(t, b) {
		t.Error("two accounts share an access token")
	}
	if storedPair(t, m, "alice").Refresh == storedPair(t, m, "bob").Refresh {
		t.Error("two accounts share a refresh token")
	}
	if a2 := authCodeFor(t, m, cfg, authCodeSession("alice", "boot-a")); a2 != a {
		t.Error("the same account and secrets got a second authenticator")
	}
}

func TestAuthCodeRedactsTokens(t *testing.T) {
	rs := newRefreshServer(t, "boot", true)
	var seen []string
	m, err := NewManager(ManagerOptions{Clock: clock.NewFake(start), Store: newMemStore(), OnToken: func(s string) { seen = append(seen, s) }})
	if err != nil {
		t.Fatal(err)
	}
	m.SetSender(&directSender{})
	authorization(t, authCodeFor(t, m, authCodeAuth(rs.URL+"/token", "header"), authCodeSession("home", "boot")))
	for _, want := range []string{"access-1", "refresh-1"} {
		if !strings.Contains(strings.Join(seen, " "), want) {
			t.Errorf("%s not registered for redaction: %v", want, seen)
		}
	}
}

func TestAuthCodeShortLivedToken(t *testing.T) {
	// A token shorter-lived than refreshBefore is renewed halfway through
	// its lifetime, not at every request.
	rs := newRefreshServer(t, "boot", false)
	rs.expiresIn = `, "expires_in": 120`
	clk := clock.NewFake(start)
	m, _ := newManager(t, clk, newMemStore(), "k")
	a := authCodeFor(t, m, authCodeAuth(rs.URL+"/token", "header"), authCodeSession("home", "boot"))
	authorization(t, a)
	clk.Advance(30 * time.Second)
	if got := authorization(t, a); got != "Bearer access-1" || rs.requests() != 1 {
		t.Fatalf("after 30s: %q, %d requests", got, rs.requests())
	}
	clk.Advance(31 * time.Second)
	if got := authorization(t, a); got != "Bearer access-2" {
		t.Errorf("after 61s: %q", got)
	}
}
