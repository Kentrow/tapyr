package sim

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/kentrow/tapyr/internal/auth"
	"github.com/kentrow/tapyr/internal/clock"
	"github.com/kentrow/tapyr/internal/config"
	"github.com/kentrow/tapyr/internal/secret"
)

const authCodeProvider = `{ type: oauth2_authorization_code, oauth2AuthorizationCode: {
  authorizeURL: "http://127.0.0.1/oauth/authorize", tokenURL: "http://127.0.0.1/oauth/token",
  deviceAuthorizationURL: "http://127.0.0.1/oauth/device",
  clientId: app, clientSecret: s3cr3t, refreshToken: { secret: refreshToken } } }`

const authCodeAccept = `  auth:
    accept:
      authorizationCode:
        - { clientId: app, clientSecret: s3cr3t, refreshTokens: [sim-bootstrap], rotate: true }
        - { clientId: public-app, refreshTokens: [sim-public] }
    tokenLifetime: 10m
  routes: [{ path: /orgs, response: { file: orgs.json } }]
`

// authCodePkg is the package of the tests: providerDoc with the secret the
// refresh token names.
func authCodePkg(t *testing.T) *config.Package {
	t.Helper()
	doc := strings.Replace(providerDoc, "  auth: AUTH", "  secrets: [{ name: refreshToken }]\n  auth: "+authCodeProvider, 1)
	fsys := fstest.MapFS{
		"provider.yaml": &fstest.MapFile{Data: []byte(doc)},
		"sim/sim.yaml":  &fstest.MapFile{Data: []byte("apiVersion: tapyr/v1alpha1\nkind: Simulation\nmetadata: { name: acme }\nspec:\n" + authCodeAccept)},
		"sim/orgs.json": &fstest.MapFile{Data: []byte(`[{"id": "o1"}]`)},
	}
	p, err := loader(t).LoadPackage(fsys, "acme", "acme", "test")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// authCodeServer starts the simulator of a package with the
// oauth2_authorization_code auth type.
func authCodeServer(t *testing.T, clk clock.Clock) (*Server, *httptest.Server) {
	t.Helper()
	p := authCodePkg(t)
	s, err := New(p, Options{Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return s, srv
}

type tokenAnswer struct {
	status int
	body   map[string]any
}

func (a tokenAnswer) str(k string) string { s, _ := a.body[k].(string); return s }

// postToken sends a form to the token endpoint; id and sec, when set, go in
// the Authorization header.
func postToken(t *testing.T, srv *httptest.Server, path string, form url.Values, id, sec string) tokenAnswer {
	t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if id != "" {
		req.SetBasicAuth(id, sec)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out := tokenAnswer{status: resp.StatusCode, body: map[string]any{}}
	_ = json.NewDecoder(resp.Body).Decode(&out.body)
	return out
}

// authorize follows the authorization URL like a browser that consents,
// and returns the redirect it gets.
func authorize(t *testing.T, srv *httptest.Server, params url.Values) (int, *url.URL) {
	t.Helper()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/oauth/authorize?"+params.Encode(), http.NoBody)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	loc, _ := url.Parse(resp.Header.Get("Location"))
	return resp.StatusCode, loc
}

func challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func consent(t *testing.T, srv *httptest.Server, client, verifier string) string {
	t.Helper()
	status, loc := authorize(t, srv, url.Values{
		"response_type": {"code"}, "client_id": {client}, "redirect_uri": {"http://127.0.0.1:8085/callback"},
		"state": {"st-1"}, "code_challenge": {challenge(verifier)}, "code_challenge_method": {"S256"},
	})
	if status != http.StatusFound || loc.Query().Get("state") != "st-1" || loc.Query().Get("code") == "" {
		t.Fatalf("authorize: %d %v", status, loc)
	}
	if loc.Host != "127.0.0.1:8085" || loc.Path != "/callback" {
		t.Errorf("redirected to %v", loc)
	}
	return loc.Query().Get("code")
}

func exchange(code, verifier string) url.Values {
	return url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "code_verifier": {verifier},
		"redirect_uri": {"http://127.0.0.1:8085/callback"},
	}
}

func TestAuthCodeSimExchange(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, time.November, 2, 9, 0, 0, 0, time.UTC))
	s, srv := authCodeServer(t, clk)
	code := consent(t, srv, "app", "verifier-123")
	got := postToken(t, srv, "/oauth/token", exchange(code, "verifier-123"), "app", "s3cr3t")
	if got.status != http.StatusOK || got.str("access_token") == "" || got.str("refresh_token") == "" || got.body["expires_in"] != 600.0 {
		t.Fatalf("exchange: %d %v", got.status, got.body)
	}
	if status, _, _ := get(t, srv.URL+"/orgs", got.str("access_token")); status != http.StatusOK {
		t.Errorf("API with the issued access token: %d", status)
	}
	// A code is single use.
	if again := postToken(t, srv, "/oauth/token", exchange(code, "verifier-123"), "app", "s3cr3t"); again.status != http.StatusBadRequest || again.str("error") != "invalid_grant" {
		t.Errorf("reused code: %d %v", again.status, again.body)
	}
	// The access token expires after tokenLifetime.
	clk.Advance(10 * time.Minute)
	if status, _, _ := get(t, srv.URL+"/orgs", got.str("access_token")); status == http.StatusOK {
		t.Error("an expired access token is accepted")
	}
	if st := s.Stats(); st.TokensIssued != 1 || st.Routes["POST /oauth/token"] != 2 {
		t.Errorf("stats: %+v", st)
	}
}

func TestAuthCodeSimExchangeErrors(t *testing.T) {
	_, srv := authCodeServer(t, clock.NewFake(time.Date(2026, time.November, 2, 9, 0, 0, 0, time.UTC)))
	tests := []struct {
		name      string
		form      func(code string) url.Values
		id, sec   string
		status    int
		wantError string
	}{
		{name: "wrong verifier", form: func(c string) url.Values { return exchange(c, "other") }, id: "app", sec: "s3cr3t", status: 400, wantError: "invalid_grant"},
		{name: "wrong redirect", form: func(c string) url.Values {
			f := exchange(c, "verifier-123")
			f.Set("redirect_uri", "http://127.0.0.1:9999/cb")
			return f
		}, id: "app", sec: "s3cr3t", status: 400, wantError: "invalid_grant"},
		{name: "wrong client secret", form: func(c string) url.Values { return exchange(c, "verifier-123") }, id: "app", sec: "nope", status: 401, wantError: "invalid_client"},
		{name: "another client", form: func(c string) url.Values {
			f := exchange(c, "verifier-123")
			f.Set("client_id", "public-app")
			return f
		}, status: 400, wantError: "invalid_grant"},
		{name: "unknown grant", form: func(string) url.Values { return url.Values{"grant_type": {"password"}} }, id: "app", sec: "s3cr3t", status: 400, wantError: "unsupported_grant_type"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code := consent(t, srv, "app", "verifier-123")
			got := postToken(t, srv, "/oauth/token", tt.form(code), tt.id, tt.sec)
			if got.status != tt.status || got.str("error") != tt.wantError {
				t.Errorf("%d %v, want %d %s", got.status, got.body, tt.status, tt.wantError)
			}
		})
	}
}

func TestAuthCodeSimAuthorizeErrors(t *testing.T) {
	_, srv := authCodeServer(t, clock.NewFake(time.Date(2026, time.November, 2, 9, 0, 0, 0, time.UTC)))
	base := url.Values{"response_type": {"code"}, "client_id": {"app"}, "redirect_uri": {"http://127.0.0.1:8085/callback"}, "state": {"s"}}
	unknown := url.Values{}
	for k, v := range base {
		unknown[k] = v
	}
	unknown.Set("client_id", "who")
	if status, _ := authorize(t, srv, unknown); status != http.StatusBadRequest {
		t.Errorf("unknown client: %d", status)
	}
	noRedirect := url.Values{"response_type": {"code"}, "client_id": {"app"}, "state": {"s"}}
	if status, _ := authorize(t, srv, noRedirect); status != http.StatusBadRequest {
		t.Errorf("no redirect_uri: %d", status)
	}
}

func TestAuthCodeSimRefresh(t *testing.T) {
	_, srv := authCodeServer(t, clock.NewFake(time.Date(2026, time.November, 2, 9, 0, 0, 0, time.UTC)))
	refresh := func(token, id, sec string) tokenAnswer {
		form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {token}}
		if sec == "" {
			form.Set("client_id", id)
			id = ""
		}
		return postToken(t, srv, "/oauth/token", form, id, sec)
	}
	// The bootstrap token of a rotating client is spent by its first use.
	first := refresh("sim-bootstrap", "app", "s3cr3t")
	next := first.str("refresh_token")
	if first.status != http.StatusOK || next == "" || next == "sim-bootstrap" {
		t.Fatalf("first refresh: %d %v", first.status, first.body)
	}
	if spent := refresh("sim-bootstrap", "app", "s3cr3t"); spent.status != http.StatusBadRequest || spent.str("error") != "invalid_grant" {
		t.Errorf("spent bootstrap: %d %v", spent.status, spent.body)
	}
	if second := refresh(next, "app", "s3cr3t"); second.status != http.StatusOK || second.str("refresh_token") == next {
		t.Errorf("rotated token: %d %v", second.status, second.body)
	}
	// Another client cannot use it; a public client without rotation
	// keeps its refresh token.
	if other := refresh("sim-public", "app", "s3cr3t"); other.str("error") != "invalid_grant" {
		t.Errorf("token of another client: %v", other.body)
	}
	for range 2 {
		if pub := refresh("sim-public", "public-app", ""); pub.status != http.StatusOK || pub.str("refresh_token") != "" {
			t.Errorf("public client: %d %v", pub.status, pub.body)
		}
	}
}

func TestAuthCodeSimDevice(t *testing.T) {
	_, srv := authCodeServer(t, clock.NewFake(time.Date(2026, time.November, 2, 9, 0, 0, 0, time.UTC)))
	dev := postToken(t, srv, "/oauth/device", url.Values{"scope": {"read"}}, "app", "s3cr3t")
	if dev.status != http.StatusOK || dev.str("device_code") == "" || dev.str("user_code") == "" || dev.str("verification_uri") == "" || dev.body["interval"] != 5.0 {
		t.Fatalf("device authorization: %d %v", dev.status, dev.body)
	}
	poll := url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:device_code"}, "device_code": {dev.str("device_code")}}
	if first := postToken(t, srv, "/oauth/token", poll, "app", "s3cr3t"); first.status != http.StatusBadRequest || first.str("error") != "authorization_pending" {
		t.Errorf("first poll: %d %v", first.status, first.body)
	}
	second := postToken(t, srv, "/oauth/token", poll, "app", "s3cr3t")
	if second.status != http.StatusOK || second.str("refresh_token") == "" {
		t.Errorf("second poll: %d %v", second.status, second.body)
	}
	if third := postToken(t, srv, "/oauth/token", poll, "app", "s3cr3t"); third.str("error") != "invalid_grant" {
		t.Errorf("device code reused: %v", third.body)
	}
}

func TestAuthCodeSimWithClient(t *testing.T) {
	// The client side of the same definition refreshes from the bootstrap
	// token, rotates, and calls the API.
	clk := clock.NewFake(time.Date(2026, time.November, 2, 9, 0, 0, 0, time.UTC))
	s, srv := authCodeServer(t, clk)
	a := *authCodePkg(t).Provider.Spec.Auth
	o := *a.OAuth2AuthorizationCode
	o.TokenURL = "{{ .params.tokenURL }}"
	a.OAuth2AuthorizationCode = &o
	m, err := auth.NewManager(auth.ManagerOptions{Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	m.SetSender(plainSender{})
	client, err := m.For(&a, auth.Session{
		Scope: auth.Scope{Provider: "p", Account: "acme"}, Vars: map[string]string{"tokenURL": srv.URL + "/oauth/token"},
		Secrets: auth.Secrets{"refreshToken": secret.New("sim-bootstrap")},
	})
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if status := apiGet(t, client, srv.URL+"/orgs"); status != http.StatusOK {
			t.Fatalf("API: status %d", status)
		}
		clk.Advance(10 * time.Minute)
	}
	if st := s.Stats(); st.TokensIssued != 3 {
		t.Errorf("%d tokens issued, want 3", st.TokensIssued)
	}
}
