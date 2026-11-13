package sim

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kentrow/tapyr/internal/budget"
	"github.com/kentrow/tapyr/internal/config"
	"github.com/kentrow/tapyr/internal/mapping"
)

// defaultTokenLifetime is the lifetime of issued tokens without
// auth.tokenLifetime.
const defaultTokenLifetime = 10 * time.Minute

// issuer serves the token endpoint (oauth2_client_credentials) or the login
// endpoint (session_login) and checks the tokens it issued.
type issuer struct {
	s        *Server
	method   string
	path     string // absolute request path
	endpoint string // endpoint of the budget descriptor
	lifetime time.Duration
	clients  []config.SimulationClient
	logins   []config.SimulationBasicCredential
	login    *config.SessionLogin // nil for OAuth2
	response string               // login response template
	logout   string               // absolute logout path, or empty
	logoutM  string

	mu     sync.Mutex
	seq    int
	tokens map[string]issuedToken
}

type issuedToken struct {
	identity string
	issued   time.Time
	expiry   time.Time
}

// newIssuer returns the issuer of a token-based auth type, or nil.
func (s *Server) newIssuer(a *config.Auth, sa *config.SimulationAuth) (*issuer, error) {
	if a == nil || (a.Type != config.AuthOAuth2ClientCredentials && a.Type != config.AuthSessionLogin) {
		return nil, nil
	}
	is := &issuer{s: s, lifetime: defaultTokenLifetime, tokens: map[string]issuedToken{}, method: http.MethodPost}
	if sa != nil {
		is.clients, is.logins = sa.Accept.Clients, sa.Accept.Logins
		if sa.TokenLifetime > 0 {
			is.lifetime = sa.TokenLifetime.Std()
		}
		if sa.LoginResponse != "" {
			data, err := fs.ReadFile(s.pkg.FS, path.Join("sim", sa.LoginResponse))
			if err != nil {
				return nil, fmt.Errorf("login response %s: %w", sa.LoginResponse, unwrapPathError(err))
			}
			is.response = string(data)
		}
	}
	if a.Type == config.AuthOAuth2ClientCredentials {
		// The token URL renders without account params: its defaults point
		// to the simulator.
		raw, err := config.RenderTemplate("tokenURL", string(a.OAuth2ClientCredentials.TokenURL), map[string]string{})
		if err != nil {
			return nil, fmt.Errorf("auth tokenURL: %w", err)
		}
		u, err := url.Parse(raw)
		if err != nil || u.Path == "" {
			return nil, fmt.Errorf("auth tokenURL: %q has no path", raw)
		}
		is.path, is.endpoint = u.Path, u.Path
		return is, nil
	}
	l := a.SessionLogin
	is.login = l
	if l.Request.Method != "" {
		is.method = l.Request.Method
	}
	is.path, is.endpoint = s.basePath+l.Request.Path, l.Request.Path
	if l.Logout != nil {
		is.logout, is.logoutM = s.basePath+l.Logout.Path, l.Logout.Method
		if is.logoutM == "" {
			is.logoutM = http.MethodPost
		}
	}
	return is, nil
}

// handles reports whether a request is for the token, login or logout
// endpoint.
func (is *issuer) handles(req *http.Request) bool {
	return is != nil && ((req.URL.Path == is.path && req.Method == is.method) ||
		(is.logout != "" && req.URL.Path == is.logout && req.Method == is.logoutM))
}

// serveLogout closes the session of the token the request carries. wait
// applies the latency of the scenario (see Server.serve).
func (is *issuer) serveLogout(w http.ResponseWriter, req *http.Request, wait func() bool) {
	s := is.s
	token := is.tokenOf(req)
	identity, ok := is.valid(token)
	if !ok {
		if wait() {
			writeError(w, s.authFail, "invalid credentials")
		}
		return
	}
	endpoint := strings.TrimPrefix(is.logout, s.basePath)
	d := budget.Descriptor{
		Provider: string(s.pkg.Provider.Metadata.Name), Account: identity, Target: identity, Credential: identity,
		Endpoint: endpoint, Method: req.Method, AuthRequest: true,
	}
	release, refused := s.engine.Check(d)
	if refused != nil {
		s.count(func(st *Stats) { st.Refused[refused.Reason]++ })
		writeJSON(w, http.StatusTooManyRequests, nil, map[string]string{"error": "rate limited", "reason": refused.Reason})
		return
	}
	defer release()
	if !wait() {
		return
	}
	is.mu.Lock()
	delete(is.tokens, token)
	is.mu.Unlock()
	s.count(func(st *Stats) { st.Routes[req.Method+" "+endpoint]++; st.Served++; st.LoggedOut++ })
	writeJSON(w, http.StatusOK, nil, map[string]any{"success": true})
}

// expireBefore invalidates the tokens issued before t (tokenExpiry).
func (is *issuer) expireBefore(t time.Time) {
	if is == nil {
		return
	}
	is.mu.Lock()
	defer is.mu.Unlock()
	for token, it := range is.tokens {
		if it.issued.Before(t) || it.issued.Equal(t) {
			delete(is.tokens, token)
		}
	}
}

// open counts the sessions whose token has not expired nor been logged
// out.
func (is *issuer) open() int64 {
	if is == nil {
		return 0
	}
	is.mu.Lock()
	defer is.mu.Unlock()
	now := is.s.clock.Now()
	var n int64
	for _, t := range is.tokens {
		if now.Before(t.expiry) {
			n++
		}
	}
	return n
}

// serve answers a token or login request. wait applies the latency of the
// scenario after the limits counted the request (see Server.serve).
func (is *issuer) serve(w http.ResponseWriter, req *http.Request, wait func() bool) {
	s := is.s
	if is.logout != "" && req.URL.Path == is.logout && req.Method == is.logoutM {
		is.serveLogout(w, req, wait)
		return
	}
	identity, ok := is.authenticate(req)
	if !ok {
		if wait() {
			writeError(w, s.authFail, "invalid credentials")
		}
		return
	}
	s.applyScenario()
	d := budget.Descriptor{
		Provider: string(s.pkg.Provider.Metadata.Name), Account: identity, Target: identity, Credential: identity,
		Endpoint: is.endpoint, Method: req.Method, AuthRequest: true,
	}
	release, refused := s.engine.Check(d)
	if refused != nil {
		retryAfter := max(1, int64(math.Ceil(refused.Until.Sub(s.clock.Now()).Seconds())))
		s.count(func(st *Stats) { st.Refused[refused.Reason]++ })
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"Retry-After": strconv.FormatInt(retryAfter, 10)},
			map[string]string{"error": "rate limited", "reason": refused.Reason})
		return
	}
	defer release()
	if !wait() {
		return
	}
	s.count(func(st *Stats) { st.Routes[req.Method+" "+is.endpoint]++ })
	token := is.issue(identity)
	secs := int64(is.lifetime / time.Second)
	s.count(func(st *Stats) { st.Served++; st.TokensIssued++ })
	if is.login == nil {
		writeJSON(w, http.StatusOK, nil, map[string]any{"access_token": token, "token_type": "Bearer", "expires_in": secs})
		return
	}
	if is.response == "" {
		writeJSON(w, http.StatusOK, nil, map[string]any{"token": token, "expiresIn": secs})
		return
	}
	body, err := mapping.DecodeJSON([]byte(strings.NewReplacer("{token}", token, "{expiresIn}", strconv.FormatInt(secs, 10)).Replace(is.response)))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "invalid login response fixture")
		return
	}
	writeJSON(w, http.StatusOK, nil, body)
}

// authenticate checks the client credentials or the login body.
func (is *issuer) authenticate(req *http.Request) (string, bool) {
	if is.login == nil {
		if err := req.ParseForm(); err != nil || req.PostForm.Get("grant_type") != "client_credentials" {
			return "", false
		}
		id, sec := req.PostForm.Get("client_id"), req.PostForm.Get("client_secret")
		if user, pass, ok := req.BasicAuth(); ok {
			id, _ = url.QueryUnescape(user)
			sec, _ = url.QueryUnescape(pass)
		}
		return id, slices.ContainsFunc(is.clients, func(c config.SimulationClient) bool {
			return equal(id, c.ClientID) && equal(sec, c.ClientSecret)
		})
	}
	data, err := io.ReadAll(io.LimitReader(req.Body, 1<<20))
	var body map[string]any
	if err != nil || json.Unmarshal(data, &body) != nil {
		return "", false
	}
	values := make([]string, 0, len(body))
	for _, v := range body {
		if str, ok := v.(string); ok {
			values = append(values, str)
		}
	}
	for _, l := range is.logins {
		if oneOf(l.Username, values) && oneOf(l.Password, values) {
			return l.Username, true
		}
	}
	return "", false
}

func (is *issuer) issue(identity string) string {
	is.mu.Lock()
	defer is.mu.Unlock()
	is.seq++
	token := fmt.Sprintf("sim-token-%d", is.seq)
	now := is.s.clock.Now()
	is.tokens[token] = issuedToken{identity: identity, issued: now, expiry: now.Add(is.lifetime)}
	return token
}

// valid returns the identity of an issued token that has not expired.
func (is *issuer) valid(token string) (string, bool) {
	is.mu.Lock()
	defer is.mu.Unlock()
	t, ok := is.tokens[token]
	if !ok {
		return "", false
	}
	if !is.s.clock.Now().Before(t.expiry) {
		delete(is.tokens, token)
		return "", false
	}
	return t.identity, true
}

// tokenOf extracts the token a request carries, as the provider applies it.
func (is *issuer) tokenOf(req *http.Request) string {
	if is.login == nil {
		token, _ := strings.CutPrefix(req.Header.Get("Authorization"), "Bearer ")
		return token
	}
	apply := is.login.Apply
	if apply.Cookie != "" {
		c, err := req.Cookie(apply.Cookie)
		if err != nil {
			return ""
		}
		return c.Value
	}
	format := apply.Format
	if format == "" {
		format = "{token}"
	}
	prefix, suffix, _ := strings.Cut(format, "{token}")
	v := req.Header.Get(apply.Header)
	if !strings.HasPrefix(v, prefix) || !strings.HasSuffix(v, suffix) || len(v) < len(prefix)+len(suffix) {
		return ""
	}
	return v[len(prefix) : len(v)-len(suffix)]
}
