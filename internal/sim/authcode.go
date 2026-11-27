package sim

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/kentrow/tapyr/internal/budget"
	"github.com/kentrow/tapyr/internal/config"
)

// codeLifetime is how long an authorization code can be exchanged.
const codeLifetime = 10 * time.Minute

// devicePollInterval is the interval the device authorization endpoint
// asks clients to poll at, in seconds.
const devicePollInterval = 5

// authCodeFlow is the simulator side of oauth2_authorization_code: the
// authorize endpoint (an immediate consent), the device authorization
// endpoint and the grants of the token endpoint (ADR 0016).
type authCodeFlow struct {
	authorize string // absolute path of the authorize endpoint
	device    string // absolute path of the device endpoint, or empty
	clients   []config.SimulationAuthCodeClient
	// redirect is the redirectURL of the provider, the registered one of
	// the clients that list none.
	redirect string

	mu      sync.Mutex
	seq     int
	codes   map[string]codeGrant
	refresh map[string]string // refresh token -> client id
	devices map[string]*deviceGrant
}

type codeGrant struct {
	client, redirect, challenge string
	expiry                      time.Time
}

type deviceGrant struct {
	client string
	polls  int
}

func newAuthCodeFlow(o *config.OAuth2AuthorizationCode, clients []config.SimulationAuthCodeClient) (*authCodeFlow, error) {
	f := &authCodeFlow{clients: clients, codes: map[string]codeGrant{}, refresh: map[string]string{}, devices: map[string]*deviceGrant{}}
	var err error
	if f.authorize, err = templatePath("authorizeURL", o.AuthorizeURL); err != nil {
		return nil, err
	}
	if f.redirect, err = config.RenderTemplate("redirectURL", string(o.RedirectURL), map[string]string{}); err != nil {
		return nil, fmt.Errorf("auth redirectURL: %w", err)
	}
	if f.redirect == "" {
		f.redirect = config.DefaultRedirectURL
	}
	if o.DeviceAuthorizationURL != "" {
		if f.device, err = templatePath("deviceAuthorizationURL", o.DeviceAuthorizationURL); err != nil {
			return nil, err
		}
	}
	for _, c := range clients {
		for _, rt := range c.RefreshTokens {
			f.refresh[rt] = c.ClientID
		}
	}
	return f, nil
}

// templatePath is the path of a URL template rendered without account
// params: its defaults point to the simulator.
func templatePath(field string, t config.Template) (string, error) {
	raw, err := config.RenderTemplate(field, string(t), map[string]string{})
	if err != nil {
		return "", fmt.Errorf("auth %s: %w", field, err)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Path == "" {
		return "", fmt.Errorf("auth %s: %q has no path", field, raw)
	}
	return u.Path, nil
}

// handles reports whether a request is for the authorize or the device
// endpoint (the token endpoint is the issuer's).
func (f *authCodeFlow) handles(req *http.Request) bool {
	return (req.Method == http.MethodGet && req.URL.Path == f.authorize) ||
		(f.device != "" && req.Method == http.MethodPost && req.URL.Path == f.device)
}

func (f *authCodeFlow) next(prefix string) string {
	f.seq++
	return fmt.Sprintf("sim-%s-%d", prefix, f.seq)
}

// client returns the client a token or device request authenticates as:
// HTTP basic or the body, and the id alone for a public client.
func (f *authCodeFlow) client(req *http.Request) (*config.SimulationAuthCodeClient, bool) {
	id, sec := req.PostForm.Get("client_id"), req.PostForm.Get("client_secret")
	if user, pass, ok := req.BasicAuth(); ok {
		id, _ = url.QueryUnescape(user)
		sec, _ = url.QueryUnescape(pass)
	}
	for i := range f.clients {
		c := &f.clients[i]
		if equal(id, c.ClientID) && (c.ClientSecret == "" && sec == "" || c.ClientSecret != "" && equal(sec, c.ClientSecret)) {
			return c, true
		}
	}
	return nil, false
}

// registered returns the redirect URL registered for a client that equals
// the requested one: the value of the configuration, never the request's.
func (f *authCodeFlow) registered(client, requested string) (string, bool) {
	for _, c := range f.clients {
		if !equal(client, c.ClientID) {
			continue
		}
		urls := c.RedirectURLs
		if len(urls) == 0 {
			urls = []string{f.redirect}
		}
		for _, u := range urls {
			if u != "" && equal(u, requested) {
				return u, true
			}
		}
	}
	return "", false
}

// serveAuthorize consents at once: it redirects to the registered redirect
// URL with a code and the state, as a vendor does after the person accepts.
// An unknown client or an unregistered redirect URL gets an error page,
// never a redirect.
func (is *issuer) serveAuthorize(w http.ResponseWriter, req *http.Request) {
	f, q := is.code, req.URL.Query()
	target, ok := f.registered(q.Get("client_id"), q.Get("redirect_uri"))
	redirect, err := url.Parse(target)
	if q.Get("response_type") != "code" || !ok || err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	f.mu.Lock()
	code := f.next("code")
	f.codes[code] = codeGrant{
		client: q.Get("client_id"), redirect: target, challenge: q.Get("code_challenge"),
		expiry: is.s.clock.Now().Add(codeLifetime),
	}
	f.mu.Unlock()
	is.s.count(func(st *Stats) { st.Routes[req.Method+" "+f.authorize]++ })
	v := redirect.Query()
	v.Set("code", code)
	v.Set("state", q.Get("state"))
	redirect.RawQuery = v.Encode()
	w.Header().Set("Location", redirect.String())
	w.WriteHeader(http.StatusFound)
}

// serveAuthCode answers the device and token endpoints of the flow. wait
// applies the latency of the scenario after the limits counted the request
// (see Server.serve).
func (is *issuer) serveAuthCode(w http.ResponseWriter, req *http.Request, wait func() bool) {
	s, f := is.s, is.code
	oauthError := func(status int, code string) { writeJSON(w, status, nil, map[string]string{"error": code}) }
	if err := req.ParseForm(); err != nil {
		oauthError(http.StatusBadRequest, "invalid_request")
		return
	}
	c, ok := f.client(req)
	if !ok {
		if wait() {
			oauthError(http.StatusUnauthorized, "invalid_client")
		}
		return
	}
	endpoint := is.endpoint
	if req.URL.Path == f.device {
		endpoint = f.device
	}
	s.applyScenario()
	d := budget.Descriptor{
		Provider: string(s.pkg.Provider.Metadata.Name), Account: c.ClientID, Target: c.ClientID, Credential: c.ClientID,
		Endpoint: endpoint, Method: req.Method, AuthRequest: true,
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
	s.count(func(st *Stats) { st.Routes[req.Method+" "+endpoint]++ })
	if endpoint == f.device {
		f.mu.Lock()
		code := f.next("device")
		f.devices[code] = &deviceGrant{client: c.ClientID}
		user := f.next("user")
		f.mu.Unlock()
		writeJSON(w, http.StatusOK, nil, map[string]any{
			"device_code": code, "user_code": user, "verification_uri": "http://" + req.Host + f.authorize,
			"expires_in": int64(codeLifetime / time.Second), "interval": devicePollInterval,
		})
		return
	}
	form := req.PostForm
	f.mu.Lock()
	newRefresh, errCode := false, ""
	switch form.Get("grant_type") {
	case "authorization_code":
		g, found := f.codes[form.Get("code")]
		delete(f.codes, form.Get("code")) // single use, even when refused
		switch {
		case !found || g.client != c.ClientID || g.redirect != form.Get("redirect_uri") || !s.clock.Now().Before(g.expiry):
			errCode = "invalid_grant"
		case g.challenge != "" && s256(form.Get("code_verifier")) != g.challenge:
			errCode = "invalid_grant"
		default:
			newRefresh = true
		}
	case "refresh_token":
		rt := form.Get("refresh_token")
		switch {
		case f.refresh[rt] != c.ClientID:
			errCode = "invalid_grant"
		case c.Rotate:
			delete(f.refresh, rt)
			newRefresh = true
		}
	case "urn:ietf:params:oauth:grant-type:device_code":
		g, found := f.devices[form.Get("device_code")]
		switch {
		case !found || g.client != c.ClientID:
			errCode = "invalid_grant"
		case g.polls == 0:
			g.polls++
			errCode = "authorization_pending"
		default:
			delete(f.devices, form.Get("device_code"))
			newRefresh = true
		}
	default:
		errCode = "unsupported_grant_type"
	}
	refresh := ""
	if errCode == "" && newRefresh {
		refresh = f.next("refresh")
		f.refresh[refresh] = c.ClientID
	}
	f.mu.Unlock()
	if errCode != "" {
		oauthError(http.StatusBadRequest, errCode)
		return
	}
	token := is.issue(c.ClientID)
	s.count(func(st *Stats) { st.Served++; st.TokensIssued++ })
	body := map[string]any{"access_token": token, "token_type": "Bearer", "expires_in": int64(is.lifetime / time.Second)}
	if refresh != "" {
		body["refresh_token"] = refresh
	}
	writeJSON(w, http.StatusOK, nil, body)
}

// s256 is the PKCE S256 challenge of a verifier (RFC 7636).
func s256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
