package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/kentrow/tapyr/internal/config"
	"github.com/kentrow/tapyr/internal/secret"
)

// ErrInvalidGrant is a token endpoint answering invalid_grant (RFC 6749,
// section 5.2): the refresh token or the authorization code is invalid,
// expired, revoked or spent.
var ErrInvalidGrant = errors.New("the token endpoint answered invalid_grant")

// TokenResponse is a successful answer of a token endpoint.
type TokenResponse struct {
	Access secret.Value
	// Refresh is zero when the endpoint returned no refresh token.
	Refresh secret.Value
	// ExpiresIn is zero when the endpoint stated no lifetime.
	ExpiresIn time.Duration
}

// ParseTokenResponse reads the answer of a token endpoint: an access token
// on 200, ErrInvalidGrant for an invalid_grant error, a rejection of the
// credentials for another 400, 401 or 403. what names the endpoint in
// errors; response bodies never appear in them.
func ParseTokenResponse(what string, status int, body []byte) (TokenResponse, error) {
	if rejected(status) {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(body, &e) == nil && e.Error == "invalid_grant" {
			return TokenResponse{}, fmt.Errorf("%s: %w", what, ErrInvalidGrant)
		}
		return TokenResponse{}, &rejectedError{what: what, status: status}
	}
	if status != http.StatusOK {
		return TokenResponse{}, fmt.Errorf("%s answered %d", what, status)
	}
	var tr struct {
		AccessToken  string          `json:"access_token"`
		RefreshToken string          `json:"refresh_token"`
		ExpiresIn    json.RawMessage `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tr); err != nil || tr.AccessToken == "" {
		return TokenResponse{}, fmt.Errorf("%s: no access_token in the response", what)
	}
	ttl, err := expiresIn(what, tr.ExpiresIn, 0)
	if err != nil {
		return TokenResponse{}, err
	}
	out := TokenResponse{Access: secret.New(tr.AccessToken), ExpiresIn: ttl}
	if tr.RefreshToken != "" {
		out.Refresh = secret.New(tr.RefreshToken)
	}
	return out, nil
}

// authCodeState is the token pair of an oauth2_authorization_code account,
// stored sealed under authCodeKey.
type authCodeState struct {
	Access  string `json:"access_token,omitempty"`
	Refresh string `json:"refresh_token"`
	// Expiry of the access token; zero when the endpoint stated none.
	Expiry time.Time `json:"expiry"`
	// Lifetime of the access token, to renew a short-lived one halfway.
	Lifetime time.Duration `json:"lifetime,omitempty"`
	// Bootstrap is the hash of the bootstrap refresh token the pair descends
	// from: a changed bootstrap secret replaces the pair.
	Bootstrap string `json:"bootstrap"`
	// Reauth is set when the token endpoint answered invalid_grant.
	Reauth bool `json:"reauth,omitempty"`
}

// authCodeLifetime is how long a stored pair is kept: refresh tokens of
// most APIs live for weeks or months without use.
const authCodeLifetime = 365 * 24 * time.Hour

// authCodeKey is the store key of the token pair of an account: provider
// and account only, so that a package upgrade that changes the auth block
// keeps the pair (ADR 0016).
func authCodeKey(provider, account string) string {
	return "authcode|" + provider + "|" + account
}

// authCode renews the access token of an account from its refresh token,
// with the rules of ADR 0016. Refreshes are serialized: the lock is held
// during a refresh.
type authCode struct {
	m        *Manager
	scope    Scope
	key      string
	tokenURL *url.URL
	o        *config.OAuth2AuthorizationCode
	apply    func(req *http.Request, token string)

	clientID, clientSecret, bootstrap secret.Value
	bootHash                          string

	mu     sync.Mutex
	loaded bool
	st     authCodeState
	// unsaved is set when the pair in memory could not be stored: it is
	// stored before its access token is used.
	unsaved    bool
	rejections int
	retryAt    time.Time
	rejection  error
}

// authCode returns the authenticator of an oauth2_authorization_code
// account, shared per account and secret values (a changed secret, after a
// new tapyr auth login, opens a new authenticator).
func (m *Manager) authCode(o *config.OAuth2AuthorizationCode, s Session) (Authenticator, error) {
	raw, err := config.RenderTemplate("tokenURL", string(o.TokenURL), s.Vars)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("auth tokenURL: %q is not an absolute http or https URL", raw)
	}
	a := &authCode{m: m, scope: s.Scope, key: authCodeKey(s.Scope.Provider, s.Scope.Account), tokenURL: u, o: o}
	if a.clientID, err = resolve(o.ClientID, s.Secrets); err != nil {
		return nil, fmt.Errorf("auth clientId: %w", err)
	}
	// clientSecret is optional: a public client (PKCE alone) leaves the
	// secret it names unset.
	if cs := o.ClientSecret; cs != nil && (cs.Secret == "" || !s.Secrets[cs.Secret].IsZero()) {
		if a.clientSecret, err = resolve(*cs, s.Secrets); err != nil {
			return nil, fmt.Errorf("auth clientSecret: %w", err)
		}
	}
	if a.bootstrap, err = resolve(o.RefreshToken, s.Secrets); err != nil {
		return nil, fmt.Errorf("auth refreshToken: %w", err)
	}
	if a.bootstrap.IsZero() {
		return nil, errors.New("auth refreshToken: empty (run tapyr auth login for this account)")
	}
	sum := sha256.Sum256([]byte(a.bootstrap.Reveal()))
	a.bootHash = hex.EncodeToString(sum[:])
	a.apply = func(req *http.Request, token string) { req.Header.Set("Authorization", "Bearer "+token) }
	if ap := o.Apply; ap != nil {
		a.apply = tokenApplier(ap)
	}
	h := sha256.New()
	for _, v := range []secret.Value{a.clientID, a.clientSecret, a.bootstrap} {
		fmt.Fprintf(h, "%d:%s\n", len(v.Reveal()), v.Reveal())
	}
	session := a.key + "|" + hex.EncodeToString(h.Sum(nil)[:8])
	m.mu.Lock()
	defer m.mu.Unlock()
	if ps, ok := m.pluginSessions[session]; ok {
		return ps.auth, nil
	}
	m.pluginSessions[session] = &pluginSession{scope: s.Scope, auth: a}
	return a, nil
}

// Status implements PluginAuthenticator, for Manager.Sessions.
func (a *authCode) Status() (time.Time, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.st.Access == "" {
		return time.Time{}, a.st.Reauth
	}
	return a.st.Expiry, a.st.Reauth
}

// loadLocked reads the stored pair, or starts from the bootstrap secret
// when none is stored or the bootstrap changed.
func (a *authCode) loadLocked(ctx context.Context) {
	if a.loaded {
		return
	}
	a.loaded = true
	if v, _, ok := a.m.load(ctx, a.key); ok {
		var st authCodeState
		if err := json.Unmarshal([]byte(v.Reveal()), &st); err == nil && st.Bootstrap == a.bootHash {
			a.st = st
			a.m.onTok(st.Access)
			a.m.onTok(st.Refresh)
			return
		}
		a.m.logger.Info("the bootstrap refresh token changed: the stored token pair is replaced", "component", "auth",
			"provider", a.scope.Provider, "account", a.scope.Account)
	}
	a.st = authCodeState{Refresh: a.bootstrap.Reveal(), Bootstrap: a.bootHash}
}

// saveLocked stores the pair; read-only or without a store, it keeps it in
// memory only.
func (a *authCode) saveLocked(ctx context.Context) error {
	m := a.m
	if m.store == nil || m.readOnly {
		a.unsaved = false
		return nil
	}
	if m.aead == nil {
		m.warn.Do(func() {
			m.logger.Warn("TAPYR_STATE_KEY is not set: auth tokens are stored unencrypted in the state store", "component", "auth")
		})
	}
	data, err := json.Marshal(a.st) //nolint:gosec // G117: sealed by the token store, never logged.
	if err != nil {
		return err
	}
	sealed, err := m.seal(a.key, string(data))
	if err == nil {
		err = m.store.SaveToken(ctx, a.key, sealed, m.clock.Now().Add(authCodeLifetime))
	}
	a.unsaved = err != nil
	return err
}

// Decorate implements Authenticator.
func (a *authCode) Decorate(ctx context.Context, req *http.Request) error {
	token, err := a.current(ctx)
	if err != nil {
		return err
	}
	a.apply(req, token)
	return nil
}

func (a *authCode) current(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.loadLocked(ctx)
	if a.st.Reauth {
		return "", fmt.Errorf("account %s: %w (run tapyr auth login %s)", a.scope.Account, ErrReauthRequired, a.scope.Account)
	}
	now := a.m.clock.Now()
	if a.unsaved {
		if err := a.saveLocked(ctx); err != nil {
			return "", fmt.Errorf("storing the refreshed token pair: %w", err)
		}
	}
	if a.st.Access != "" {
		margin := a.o.RefreshBefore.Std()
		if a.st.Lifetime > 0 && margin > a.st.Lifetime/2 {
			margin = a.st.Lifetime / 2
		}
		fresh := a.st.Expiry.IsZero() || now.Before(a.st.Expiry.Add(-margin))
		if fresh || (a.m.readOnly && now.Before(a.st.Expiry)) {
			return a.st.Access, nil
		}
	}
	if a.m.readOnly {
		return "", ErrRenewalNotAllowed
	}
	if now.Before(a.retryAt) {
		return "", fmt.Errorf("refreshing the access token: %w (next attempt at %s)", a.rejection, a.retryAt.UTC().Format(time.RFC3339))
	}
	tr, err := a.refresh(ctx)
	if err != nil {
		a.m.obs.TokenRequest(a.scope.Provider, a.scope.Account, "failed")
		switch re, isRejected := errors.AsType[*rejectedError](err); {
		case errors.Is(err, ErrInvalidGrant):
			a.st.Access, a.st.Expiry, a.st.Lifetime, a.st.Reauth = "", time.Time{}, 0, true
			if serr := a.saveLocked(ctx); serr != nil {
				a.m.logger.Warn("storing the re-bootstrap condition", "component", "auth", "error", serr)
			}
			a.m.logger.Error("the refresh token was refused: run tapyr auth login for this account", "component", "auth",
				"provider", a.scope.Provider, "account", a.scope.Account)
			return "", fmt.Errorf("account %s: %w (run tapyr auth login %s)", a.scope.Account, ErrReauthRequired, a.scope.Account)
		case isRejected:
			wait := min(minRejectionBackoff<<min(a.rejections, 10), maxRejectionBackoff)
			a.rejections++
			a.retryAt, a.rejection = now.Add(wait), re
			a.m.logger.Warn("client credentials rejected: the next token request waits", "component", "auth",
				"provider", a.scope.Provider, "account", a.scope.Account, "status", re.status, "wait", wait.String(),
				"rejections", a.rejections)
		}
		return "", fmt.Errorf("refreshing the access token: %w", err)
	}
	a.rejections, a.retryAt, a.rejection = 0, time.Time{}, nil
	a.m.obs.TokenRequest(a.scope.Provider, a.scope.Account, "success")
	a.m.onTok(tr.Access.Reveal())
	a.st.Access, a.st.Expiry, a.st.Lifetime = tr.Access.Reveal(), time.Time{}, tr.ExpiresIn
	if tr.ExpiresIn > 0 {
		a.st.Expiry = now.Add(tr.ExpiresIn)
	}
	if !tr.Refresh.IsZero() {
		a.m.onTok(tr.Refresh.Reveal())
		a.st.Refresh = tr.Refresh.Reveal()
	}
	// The pair is stored before the access token is used: a rotated
	// refresh token lost in a crash means a new tapyr auth login.
	if err := a.saveLocked(ctx); err != nil {
		return "", fmt.Errorf("storing the refreshed token pair: %w", err)
	}
	return a.st.Access, nil
}

// refresh sends the refresh_token grant.
func (a *authCode) refresh(ctx context.Context) (TokenResponse, error) {
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {a.st.Refresh}}
	for k, v := range a.o.TokenParams {
		form.Set(k, v)
	}
	req, err := NewTokenRequest(ctx, a.tokenURL, form, a.o.AuthStyle, a.clientID, a.clientSecret)
	if err != nil {
		return TokenResponse{}, err
	}
	status, body, err := a.m.do(ctx, a.scope, a.tokenURL.Path, req)
	if err != nil {
		return TokenResponse{}, err
	}
	return ParseTokenResponse("token endpoint "+a.tokenURL.Path, status, body)
}

// NewTokenRequest builds a form POST to a token endpoint with the client
// authentication of authStyle: HTTP basic (header) or the body. A public
// client (no secret) sends its id in the body.
func NewTokenRequest(ctx context.Context, u *url.URL, form url.Values, authStyle string, id, sec secret.Value) (*http.Request, error) {
	basic := authStyle != "body" && !sec.IsZero()
	if !basic {
		form.Set("client_id", id.Reveal())
		if !sec.IsZero() {
			form.Set("client_secret", sec.Reveal())
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if basic {
		// RFC 6749 section 2.3.1: form-encoded, then basic.
		req.SetBasicAuth(url.QueryEscape(id.Reveal()), url.QueryEscape(sec.Reveal()))
	}
	return req, nil
}

// OnAuthFailure implements Authenticator: the access token is dropped and
// the request retried once after a refresh, unless the refresh token is
// already refused.
func (a *authCode) OnAuthFailure(context.Context, *http.Response) (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.st.Reauth {
		return false, nil
	}
	a.st.Access, a.st.Expiry, a.st.Lifetime = "", time.Time{}, 0
	return true, nil
}
