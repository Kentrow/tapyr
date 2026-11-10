package auth

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kentrow/tapyr/internal/clock"
	"github.com/kentrow/tapyr/internal/config"
	"github.com/kentrow/tapyr/internal/mapping"
	"github.com/kentrow/tapyr/internal/secret"
)

const (
	// DefaultTokenLifetime applies when a token response states no lifetime:
	// the token is renewed after it, or sooner when rejected.
	DefaultTokenLifetime = time.Hour
	// DefaultRefreshBefore renews tokens this long before they expire.
	DefaultRefreshBefore = 30 * time.Second
	// maxTokenResponse caps the size of token and login responses.
	maxTokenResponse = 1 << 20
	// After a rejection of the credentials, token and login requests wait
	// minRejectionBackoff, doubling up to maxRejectionBackoff, so that a
	// wrong password does not hit the API (or lock the account) at every
	// run.
	minRejectionBackoff = time.Minute
	maxRejectionBackoff = time.Hour
)

// rejectedError is a token or login request answered 400, 401 or 403: the
// credentials are wrong, retrying at once will not help.
type rejectedError struct {
	what   string // "token endpoint /oauth/token" or "login /login"
	status int
}

func (e *rejectedError) Error() string {
	return fmt.Sprintf("%s answered %d: the credentials were rejected", e.what, e.status)
}

func rejected(status int) bool {
	return status == http.StatusBadRequest || status == http.StatusUnauthorized || status == http.StatusForbidden
}

// Scope identifies the account and target a token request is made for, for
// the budget engine.
type Scope struct {
	Provider   string
	Account    string
	Target     string
	Credential string
}

// Sender sends token and login requests through the budget engine, as auth
// requests. endpoint is the path of the request.
type Sender interface {
	SendAuth(ctx context.Context, scope Scope, endpoint string, req *http.Request) (*http.Response, error)
}

// TokenStore persists tokens, sealed by the Manager.
type TokenStore interface {
	LoadToken(ctx context.Context, key string) (sealed []byte, expiry time.Time, ok bool, err error)
	SaveToken(ctx context.Context, key string, sealed []byte, expiry time.Time) error
	DeleteToken(ctx context.Context, key string) error
}

// ManagerOptions configures a Manager.
type ManagerOptions struct {
	Clock  clock.Clock
	Logger *slog.Logger
	// Store persists tokens across restarts; nil keeps them in memory.
	Store TokenStore
	// StateKey encrypts the stored tokens (AES-GCM, key derived with
	// SHA-256). Empty stores them unencrypted, with a warning logged once.
	StateKey string
	// Observer receives the token requests (self-metrics); optional.
	Observer Observer
	// OnToken is called with every token obtained or loaded, so that logs
	// redact it; optional.
	OnToken func(token string)
	// Plugins are the auth plugins available to providers.
	Plugins Plugins
	// ReadOnly is for commands that run next to tapyr run on the same state
	// store (tapyr record): the store is never written, plugins read their
	// stored state but must not renew credentials whose renewal invalidates
	// the previous ones (PluginEnv.ReadOnly), and other tokens are obtained
	// in memory, so that a logout never ends a session of tapyr run.
	ReadOnly bool
}

// Observer receives the token and login requests of a Manager: result is
// success or failed.
type Observer interface {
	TokenRequest(provider, account, result string)
}

type nopObserver struct{}

func (nopObserver) TokenRequest(string, string, string) {}

// SessionInfo describes a token session.
type SessionInfo struct {
	Provider, Account, Target string
	// Expiry is zero when no token is held.
	Expiry time.Time
	// Reauth reports that the credentials need a manual re-bootstrap.
	Reauth bool
}

// Manager shares token sessions between the jobs of an account and
// persists their tokens. The Sender is set once the executor exists.
type Manager struct {
	clock  clock.Clock
	logger *slog.Logger
	store  TokenStore
	aead   cipher.AEAD
	warn   sync.Once
	obs    Observer
	onTok  func(string)

	plugins  Plugins
	readOnly bool

	mu             sync.Mutex
	sender         Sender
	sessions       map[string]*session
	pluginSessions map[string]*pluginSession
}

// NewManager returns a Manager.
func NewManager(opts ManagerOptions) (*Manager, error) {
	if opts.Clock == nil {
		opts.Clock = clock.Real()
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	m := &Manager{
		clock: opts.Clock, logger: opts.Logger, store: opts.Store, sessions: map[string]*session{},
		pluginSessions: map[string]*pluginSession{}, obs: opts.Observer, onTok: opts.OnToken, plugins: opts.Plugins,
		readOnly: opts.ReadOnly,
	}
	if m.obs == nil {
		m.obs = nopObserver{}
	}
	if m.onTok == nil {
		m.onTok = func(string) {}
	}
	if opts.StateKey != "" {
		key := sha256.Sum256([]byte(opts.StateKey))
		block, err := aes.NewCipher(key[:])
		if err != nil {
			return nil, fmt.Errorf("state key: %w", err)
		}
		if m.aead, err = cipher.NewGCM(block); err != nil {
			return nil, fmt.Errorf("state key: %w", err)
		}
	}
	return m, nil
}

// Sessions describes the token sessions, sorted by key.
func (m *Manager) Sessions() []SessionInfo {
	m.mu.Lock()
	sessions := make([]*session, 0, len(m.sessions))
	for _, key := range slices.Sorted(maps.Keys(m.sessions)) {
		sessions = append(sessions, m.sessions[key])
	}
	plugins := make([]*pluginSession, 0, len(m.pluginSessions))
	for _, key := range slices.Sorted(maps.Keys(m.pluginSessions)) {
		plugins = append(plugins, m.pluginSessions[key])
	}
	m.mu.Unlock()
	out := make([]SessionInfo, 0, len(sessions)+len(plugins))
	for _, s := range sessions {
		s.mu.Lock()
		info := SessionInfo{Provider: s.scope.Provider, Account: s.scope.Account, Target: s.scope.Target}
		if !s.token.IsZero() {
			info.Expiry = s.expiry
		}
		s.mu.Unlock()
		out = append(out, info)
	}
	for _, p := range plugins {
		info := SessionInfo{Provider: p.scope.Provider, Account: p.scope.Account, Target: p.scope.Target}
		info.Expiry, info.Reauth = p.auth.Status()
		out = append(out, info)
	}
	return out
}

// SetSender sets the sender of token requests.
func (m *Manager) SetSender(s Sender) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sender = s
}

func (m *Manager) send(ctx context.Context, scope Scope, endpoint string, req *http.Request) (*http.Response, error) {
	m.mu.Lock()
	s := m.sender
	m.mu.Unlock()
	if s == nil {
		return nil, errors.New("token requests are not available in this command")
	}
	return s.SendAuth(ctx, scope, endpoint, req)
}

// Session describes the account (or target) an authenticator is created for.
type Session struct {
	Scope   Scope
	BaseURL *url.URL
	Secrets Secrets
	// Vars, Account and Target are the CEL variables of login body
	// expressions.
	Vars    map[string]string
	Account map[string]any
	Target  map[string]any
	// Login holds the compiled sessionLogin expressions.
	Login *LoginPrograms
}

// LoginPrograms are the compiled expressions of a sessionLogin block.
type LoginPrograms struct {
	Token     *mapping.Program
	ExpiresIn *mapping.Program
	Body      map[string]*mapping.Program
}

// For returns the authenticator of a provider auth block for a session:
// static types as New, token types shared per credential (and per target
// with perTarget).
func (m *Manager) For(a *config.Auth, s Session) (Authenticator, error) {
	if a == nil {
		return New(a, s.Secrets)
	}
	switch a.Type {
	case config.AuthOAuth2ClientCredentials:
		return m.oauth2(a.OAuth2ClientCredentials, s)
	case config.AuthSessionLogin:
		return m.login(a.SessionLogin, s)
	case config.AuthPlugin:
		return m.plugin(a.Plugin, s)
	}
	return New(a, s.Secrets)
}

// shared returns the session of key, created by mk when absent.
func (m *Manager) shared(key string, mk func() *session) *session {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sessions[key]; ok {
		return s
	}
	s := mk()
	s.m, s.key = m, key
	m.sessions[key] = s
	return s
}

func (m *Manager) oauth2(o *config.OAuth2ClientCredentials, s Session) (Authenticator, error) {
	tokenURL, err := config.RenderTemplate("tokenURL", string(o.TokenURL), s.Vars)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(tokenURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("auth tokenURL: %q is not an absolute http or https URL", tokenURL)
	}
	id, err := resolve(o.ClientID, s.Secrets)
	if err != nil {
		return nil, fmt.Errorf("auth clientId: %w", err)
	}
	sec, err := resolve(o.ClientSecret, s.Secrets)
	if err != nil {
		return nil, fmt.Errorf("auth clientSecret: %w", err)
	}
	key := strings.Join([]string{"oauth2", s.Scope.Provider, s.Scope.Credential, fingerprint(o, u.String())}, "|")
	return m.shared(key, func() *session {
		return &session{
			scope: s.Scope, refreshBefore: DefaultRefreshBefore,
			fetch: func(ctx context.Context, _ time.Time) (string, time.Duration, error) {
				return m.fetchOAuth2(ctx, s.Scope, u, o, id, sec)
			},
			apply: func(req *http.Request, token string) { req.Header.Set("Authorization", "Bearer "+token) },
		}
	}), nil
}

func (m *Manager) fetchOAuth2(ctx context.Context, scope Scope, u *url.URL, o *config.OAuth2ClientCredentials,
	id, sec secret.Value,
) (string, time.Duration, error) {
	form := url.Values{"grant_type": {"client_credentials"}}
	if len(o.Scopes) > 0 {
		form.Set("scope", strings.Join(o.Scopes, " "))
	}
	if o.Audience != "" {
		form.Set("audience", o.Audience)
	}
	for k, v := range o.ExtraParams {
		form.Set(k, v)
	}
	if o.AuthStyle == "body" {
		form.Set("client_id", id.Reveal())
		form.Set("client_secret", sec.Reveal())
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if o.AuthStyle != "body" {
		// RFC 6749 section 2.3.1: form-encoded, then basic.
		req.SetBasicAuth(url.QueryEscape(id.Reveal()), url.QueryEscape(sec.Reveal()))
	}
	status, body, err := m.do(ctx, scope, u.Path, req)
	if err != nil {
		return "", 0, err
	}
	if rejected(status) {
		return "", 0, &rejectedError{what: "token endpoint " + u.Path, status: status}
	}
	if status != http.StatusOK {
		return "", 0, fmt.Errorf("token endpoint %s answered %d", u.Path, status)
	}
	var tr struct {
		AccessToken string          `json:"access_token"`
		ExpiresIn   json.RawMessage `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tr); err != nil || tr.AccessToken == "" {
		return "", 0, fmt.Errorf("token endpoint %s: no access_token in the response", u.Path)
	}
	ttl := DefaultTokenLifetime
	if raw := strings.Trim(string(tr.ExpiresIn), `"`); raw != "" && raw != "null" {
		secs, err := strconv.ParseFloat(raw, 64)
		if err != nil || secs <= 0 {
			return "", 0, fmt.Errorf("token endpoint %s: invalid expires_in", u.Path)
		}
		ttl = time.Duration(secs * float64(time.Second))
	}
	return tr.AccessToken, ttl, nil
}

func (m *Manager) login(l *config.SessionLogin, s Session) (Authenticator, error) {
	if s.BaseURL == nil || s.Login == nil || s.Login.Token == nil {
		return nil, errors.New("auth sessionLogin: the login is not compiled")
	}
	body := map[string]any{}
	exprs := map[string]*mapping.Program{}
	for k, v := range l.Request.Body {
		switch {
		case v.Expr != "":
			exprs[k] = s.Login.Body[k]
		default:
			val, err := resolve(v, s.Secrets)
			if err != nil {
				return nil, fmt.Errorf("auth sessionLogin body %s: %w", k, err)
			}
			body[k] = val
		}
	}
	parts := []string{"login", s.Scope.Provider, s.Scope.Account, s.Scope.Credential, fingerprint(l, s.BaseURL.String())}
	if l.PerTarget {
		parts = append(parts, s.Scope.Target)
	}
	refresh := DefaultRefreshBefore
	if l.RefreshBefore > 0 {
		refresh = l.RefreshBefore.Std()
	}
	u := *s.BaseURL
	u.Path = strings.TrimSuffix(u.Path, "/") + l.Request.Path
	return m.shared(strings.Join(parts, "|"), func() *session {
		sess := &session{
			scope: s.Scope, refreshBefore: refresh,
			fetch: func(ctx context.Context, now time.Time) (string, time.Duration, error) {
				return m.fetchLogin(ctx, s, &u, l, body, exprs, now)
			},
			apply: func(req *http.Request, token string) {
				if l.Apply.Cookie != "" {
					// A request cookie: Secure, HttpOnly and SameSite are
					// attributes of response cookies.
					req.AddCookie(&http.Cookie{Name: l.Apply.Cookie, Value: token}) //nolint:gosec // G124: see above.
					return
				}
				format := l.Apply.Format
				if format == "" {
					format = "{token}"
				}
				req.Header.Set(l.Apply.Header, strings.ReplaceAll(format, "{token}", token))
			},
		}
		if l.Logout != nil {
			out := *s.BaseURL
			out.Path = strings.TrimSuffix(out.Path, "/") + l.Logout.Path
			sess.logout = func(ctx context.Context, token string) {
				m.sendLogout(ctx, s.Scope, &out, l.Logout, func(req *http.Request) { sess.apply(req, token) })
			}
		}
		return sess
	}), nil
}

// sendLogout closes a session. Failures are logged at debug level: the
// session expires on its own.
func (m *Manager) sendLogout(ctx context.Context, scope Scope, u *url.URL, l *config.LogoutRequest, apply func(*http.Request)) {
	method := l.Method
	if method == "" {
		method = http.MethodPost
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), http.NoBody)
	if err != nil {
		return
	}
	req.Header.Set("Accept", "application/json")
	apply(req)
	status, _, err := m.do(ctx, scope, l.Path, req)
	if err == nil && (status < 200 || status > 299) {
		err = fmt.Errorf("logout %s answered %d", l.Path, status)
	}
	if err != nil {
		m.logger.Debug("closing a session", "component", "auth", "provider", scope.Provider, "account", scope.Account,
			"target", scope.Target, "error", err)
	}
}

// Close logs out the sessions of the providers that declare a logout, so
// that Tapyr leaves no session open when it stops. The sender must still be
// available.
func (m *Manager) Close(ctx context.Context) {
	m.mu.Lock()
	sessions := make([]*session, 0, len(m.sessions))
	for _, key := range slices.Sorted(maps.Keys(m.sessions)) {
		sessions = append(sessions, m.sessions[key])
	}
	m.mu.Unlock()
	for _, s := range sessions {
		s.close(ctx)
	}
}

func (m *Manager) fetchLogin(ctx context.Context, s Session, u *url.URL, l *config.SessionLogin,
	values map[string]any, exprs map[string]*mapping.Program, now time.Time,
) (string, time.Duration, error) {
	body := make(map[string]any, len(values)+len(exprs))
	for k, v := range values {
		body[k] = v.(secret.Value).Reveal()
	}
	for k, p := range exprs {
		v, err := p.Eval(&mapping.Activation{Vars: s.Vars, Account: s.Account, Target: s.Target, Now: now})
		var str string
		if err == nil {
			str, err = mapping.ToLabel(v)
		}
		if err != nil {
			return "", 0, fmt.Errorf("auth sessionLogin body %s: %w", k, err)
		}
		body[k] = str
	}
	data, err := json.Marshal(body)
	if err != nil {
		return "", 0, err
	}
	method := l.Request.Method
	if method == "" {
		method = http.MethodPost
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), strings.NewReader(string(data)))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	for k, v := range l.Request.Headers {
		req.Header.Set(k, v)
	}
	status, raw, err := m.do(ctx, s.Scope, l.Request.Path, req)
	if err != nil {
		return "", 0, err
	}
	if rejected(status) {
		return "", 0, &rejectedError{what: "login " + l.Request.Path, status: status}
	}
	if status < 200 || status > 299 {
		return "", 0, fmt.Errorf("login %s answered %d", l.Request.Path, status)
	}
	page := mapping.Page{Status: status}
	if page.Body, err = mapping.DecodeJSON(raw); err != nil {
		return "", 0, fmt.Errorf("login %s: %w", l.Request.Path, err)
	}
	act := &mapping.Activation{Response: page.Value(), Now: now}
	v, err := s.Login.Token.Eval(act)
	var token string
	if err == nil {
		token, err = mapping.ToLabel(v)
	}
	if err != nil || token == "" {
		return "", 0, fmt.Errorf("login %s: the token expression returned no token", l.Request.Path)
	}
	ttl := DefaultTokenLifetime
	if p := s.Login.ExpiresIn; p != nil {
		v, err := p.Eval(act)
		if err != nil {
			return "", 0, fmt.Errorf("login %s: expiresIn: %w", l.Request.Path, err)
		}
		d, ok := v.Value().(time.Duration)
		if !ok || d <= 0 {
			return "", 0, fmt.Errorf("login %s: expiresIn is not a positive duration", l.Request.Path)
		}
		ttl = d
	}
	return token, ttl, nil
}

// fingerprint identifies an auth configuration in session keys, so that a
// changed configuration (after a reload) opens a new session. It is a hash:
// literal values of the block never reach the stored keys.
func fingerprint(block any, rendered string) string {
	data, _ := json.Marshal(block)
	sum := sha256.Sum256(append(data, rendered...))
	return hex.EncodeToString(sum[:8])
}

// do sends a token request and reads its response. Response bodies never
// appear in errors: they may carry tokens.
func (m *Manager) do(ctx context.Context, scope Scope, endpoint string, req *http.Request) (int, []byte, error) {
	resp, err := m.send(ctx, scope, endpoint, req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxTokenResponse))
	if err != nil {
		return 0, nil, fmt.Errorf("reading the %s response: %w", endpoint, err)
	}
	return resp.StatusCode, data, nil
}

// load returns a stored token, or false.
func (m *Manager) load(ctx context.Context, key string) (secret.Value, time.Time, bool) {
	if m.store == nil {
		return secret.Value{}, time.Time{}, false
	}
	sealed, expiry, ok, err := m.store.LoadToken(ctx, key)
	if err != nil {
		m.logger.Warn("loading a stored token", "component", "auth", "error", err)
		return secret.Value{}, time.Time{}, false
	}
	if !ok {
		return secret.Value{}, time.Time{}, false
	}
	token, err := m.open(key, sealed)
	if err != nil {
		// Encrypted with another key, or stored before a key was set.
		m.logger.Info("ignoring a stored token", "component", "auth", "reason", err.Error())
		return secret.Value{}, time.Time{}, false
	}
	return secret.New(token), expiry, true
}

func (m *Manager) save(ctx context.Context, key string, token secret.Value, expiry time.Time) {
	if m.store == nil || m.readOnly {
		return
	}
	if m.aead == nil {
		m.warn.Do(func() {
			m.logger.Warn("TAPYR_STATE_KEY is not set: auth tokens are stored unencrypted in the state store", "component", "auth")
		})
	}
	sealed, err := m.seal(key, token.Reveal())
	if err == nil {
		err = m.store.SaveToken(ctx, key, sealed, expiry)
	}
	if err != nil {
		m.logger.Warn("storing a token", "component", "auth", "error", err)
	}
}

func (m *Manager) forget(ctx context.Context, key string) {
	if m.store == nil || m.readOnly {
		return
	}
	if err := m.store.DeleteToken(ctx, key); err != nil {
		m.logger.Warn("deleting a stored token", "component", "auth", "error", err)
	}
}

// Stored token formats: a version byte, then the token (plain) or the
// nonce and the AES-GCM ciphertext, authenticated with the session key.
const (
	formatPlain  = 0
	formatSealed = 1
)

func (m *Manager) seal(key, token string) ([]byte, error) {
	if m.aead == nil {
		return append([]byte{formatPlain}, token...), nil
	}
	nonce := make([]byte, m.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	out := append([]byte{formatSealed}, nonce...)
	return m.aead.Seal(out, nonce, []byte(token), []byte(key)), nil
}

func (m *Manager) open(key string, sealed []byte) (string, error) {
	if len(sealed) == 0 {
		return "", errors.New("empty token")
	}
	switch sealed[0] {
	case formatPlain:
		if m.aead != nil {
			return "", errors.New("stored unencrypted while TAPYR_STATE_KEY is set")
		}
		return string(sealed[1:]), nil
	case formatSealed:
		if m.aead == nil {
			return "", errors.New("encrypted, and TAPYR_STATE_KEY is not set")
		}
		ns := m.aead.NonceSize()
		if len(sealed) < 1+ns {
			return "", errors.New("truncated token")
		}
		plain, err := m.aead.Open(nil, sealed[1:1+ns], sealed[1+ns:], []byte(key))
		if err != nil {
			return "", errors.New("cannot decrypt with TAPYR_STATE_KEY")
		}
		return string(plain), nil
	}
	return "", errors.New("unknown token format")
}

// session is a token shared by the jobs of an account: obtained on first
// use, renewed refreshBefore its expiry or when rejected. Renewals are
// serialized.
type session struct {
	m             *Manager
	key           string
	scope         Scope
	refreshBefore time.Duration
	fetch         func(ctx context.Context, now time.Time) (token string, ttl time.Duration, err error)
	apply         func(req *http.Request, token string)
	// logout closes the session of a token; nil when the provider declares
	// no logout.
	logout func(ctx context.Context, token string)

	mu     sync.Mutex
	loaded bool
	token  secret.Value
	expiry time.Time
	// lifetime of the token when obtained by this process (0 when loaded
	// from the store).
	lifetime time.Duration
	// rejections counts the consecutive rejections of the credentials;
	// no request is sent before retryAt.
	rejections int
	retryAt    time.Time
	rejection  error
}

// Decorate implements Authenticator.
func (s *session) Decorate(ctx context.Context, req *http.Request) error {
	token, err := s.current(ctx)
	if err != nil {
		return err
	}
	s.apply(req, token.Reveal())
	return nil
}

func (s *session) current(ctx context.Context) (secret.Value, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.m.clock.Now()
	// Read-only, a stored token is not used: its session belongs to tapyr
	// run, and a logout (on renewal or Close) would end it.
	if !s.loaded {
		s.loaded = true
		if !s.m.readOnly {
			s.token, s.expiry, _ = s.m.load(ctx, s.key)
		}
		if !s.token.IsZero() {
			s.m.onTok(s.token.Reveal())
		}
	}
	if !s.token.IsZero() && now.Before(s.renewAt()) {
		return s.token, nil
	}
	if now.Before(s.retryAt) {
		return secret.Value{}, fmt.Errorf("obtaining a token: %w (next attempt at %s)", s.rejection, s.retryAt.UTC().Format(time.RFC3339))
	}
	token, ttl, err := s.fetch(ctx, now)
	if err != nil {
		s.m.obs.TokenRequest(s.scope.Provider, s.scope.Account, "failed")
		if re, ok := errors.AsType[*rejectedError](err); ok {
			wait := min(minRejectionBackoff<<min(s.rejections, 10), maxRejectionBackoff)
			s.rejections++
			s.retryAt, s.rejection = now.Add(wait), re
			s.m.logger.Warn("credentials rejected: the next token request waits", "component", "auth",
				"provider", s.scope.Provider, "account", s.scope.Account, "target", s.scope.Target,
				"status", re.status, "wait", wait.String(), "rejections", s.rejections)
		}
		return secret.Value{}, fmt.Errorf("obtaining a token: %w", err)
	}
	s.rejections, s.retryAt, s.rejection = 0, time.Time{}, nil
	s.m.obs.TokenRequest(s.scope.Provider, s.scope.Account, "success")
	s.m.onTok(token)
	old, oldExpiry := s.token, s.expiry
	s.token, s.expiry, s.lifetime = secret.New(token), now.Add(ttl), ttl
	s.m.save(ctx, s.key, s.token, s.expiry)
	if s.logout != nil && !old.IsZero() && now.Before(oldExpiry) {
		s.logout(ctx, old.Reveal())
	}
	return s.token, nil
}

// close logs the session out, when it holds a live token and the provider
// declares a logout, and forgets its token.
func (s *session) close(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.logout == nil || s.token.IsZero() || !s.m.clock.Now().Before(s.expiry) {
		return
	}
	s.logout(ctx, s.token.Reveal())
	s.token, s.expiry, s.lifetime = secret.Value{}, time.Time{}, 0
	s.m.forget(ctx, s.key)
}

// renewAt is when the token is renewed: refreshBefore its expiry, at most
// halfway through its lifetime for short-lived tokens.
func (s *session) renewAt() time.Time {
	margin := s.refreshBefore
	if s.lifetime > 0 && margin > s.lifetime/2 {
		margin = s.lifetime / 2
	}
	return s.expiry.Add(-margin)
}

// OnAuthFailure implements Authenticator: the token is dropped and the
// request retried once with a new one.
func (s *session) OnAuthFailure(ctx context.Context, _ *http.Response) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loaded = true
	s.token, s.expiry, s.lifetime = secret.Value{}, time.Time{}, 0
	s.m.forget(ctx, s.key)
	return true, nil
}
