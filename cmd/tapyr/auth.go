package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/kentrow/tapyr/internal/auth"
	"github.com/kentrow/tapyr/internal/budget"
	"github.com/kentrow/tapyr/internal/collect"
	"github.com/kentrow/tapyr/internal/config"
	"github.com/kentrow/tapyr/internal/secret"
	"github.com/kentrow/tapyr/internal/selfobs"
)

const (
	// loginTimeout bounds the wait for the browser of --listen and the
	// device flow.
	loginTimeout = 15 * time.Minute
	// maxLoginResponse caps the responses read by the login.
	maxLoginResponse = 1 << 20
	// deviceGrant is the grant type of RFC 8628.
	deviceGrant = "urn:ietf:params:oauth:grant-type:device_code"
)

func newAuthCmd(e *env, flags *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "auth",
		Short: "Obtain the credentials of accounts",
	}
	cmd.AddCommand(newAuthLoginCmd(e, flags))
	return cmd
}

// loginOptions are the flags of tapyr auth login.
type loginOptions struct {
	secretFile     string
	force          bool
	listen, device bool
}

func newAuthLoginCmd(e *env, flags *globalFlags) *cobra.Command {
	var o loginOptions
	cmd := &cobra.Command{
		Use:   "login <account>",
		Short: "Run the consent of an oauth2_authorization_code account and write its refresh token",
		Long: "Run the OAuth 2.0 authorization code flow of an account once, and write the\n" +
			"refresh token it yields to --secret-file (mode 0600), for the refreshToken\n" +
			"secret of the account (ADR 0016). It prints the URL to open in a browser, on\n" +
			"any machine, then reads the URL the browser was redirected to (or the code)\n" +
			"from the standard input. --listen serves the redirect itself, for a\n" +
			"redirectURL on the loopback interface of this machine; --device uses the\n" +
			"device authorization grant of a package that declares it. It never prints a\n" +
			"token and never opens the state store: run it anywhere, then reload tapyr run.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if o.secretFile == "" {
				return &exitError{code: exitFailure, err: errors.New("--secret-file is required: the refresh token is written there, never printed")}
			}
			if o.listen && o.device {
				return &exitError{code: exitFailure, err: errors.New("use either --listen or --device")}
			}
			return authLogin(cmd.Context(), e, flags, args[0], o)
		},
	}
	cmd.Flags().StringVar(&o.secretFile, "secret-file", "", "file receiving the refresh token (created with mode 0600)")
	cmd.Flags().BoolVar(&o.force, "force", false, "overwrite an existing --secret-file")
	cmd.Flags().BoolVar(&o.listen, "listen", false, "serve the redirect URL on this machine instead of reading a pasted URL")
	cmd.Flags().BoolVar(&o.device, "device", false, "use the device authorization grant (RFC 8628)")
	addProviderPathFlag(cmd, flags)
	return cmd
}

// loginFlow is the resolved configuration of one login.
type loginFlow struct {
	scope                                          auth.Scope
	block                                          *config.OAuth2AuthorizationCode
	authorizeURL, tokenURL, deviceURL, redirectURL *url.URL
	clientID, clientSecret                         secret.Value
	send                                           func(ctx context.Context, endpoint string, req *http.Request) (*http.Response, error)
	clock                                          interface {
		Sleep(context.Context, time.Duration) error
	}
}

func authLogin(ctx context.Context, e *env, flags *globalFlags, account string, o loginOptions) error {
	cfg, err := loadConfig(e, flags)
	if err != nil {
		return err
	}
	acc, prov, err := loginAccount(cfg, account)
	if err != nil {
		return &exitError{code: exitInvalidConfig, err: err}
	}
	block := prov.Spec.Auth.OAuth2AuthorizationCode
	if o.device && block.DeviceAuthorizationURL == "" {
		return &exitError{code: exitFailure, err: fmt.Errorf("provider %s declares no deviceAuthorizationURL: --device is not available", prov.Metadata.Name)}
	}
	// The file is checked before the flow: a consent whose token cannot be
	// written would be wasted.
	if _, err := os.Stat(o.secretFile); err == nil && !o.force {
		return &exitError{code: exitFailure, err: fmt.Errorf("%s already exists: use --force to overwrite it", o.secretFile)}
	}
	redactor := selfobs.NewRedactor()
	secrets := config.SecretSource{LookupEnv: e.lookupEnv, ReadFile: os.ReadFile, OnResolve: func(v string) { redactor.Add(v) }}
	f, err := newLoginFlow(acc, prov, secrets)
	if err != nil {
		return &exitError{code: exitInvalidConfig, err: err}
	}
	logger, err := settingsLogger(e, flags, &cfg.Settings.Spec.Log, redactor)
	if err != nil {
		return &exitError{code: exitFailure, err: err}
	}
	// The token requests go through a budget engine with the limits of the
	// provider, in memory: the login opens no state store.
	engine, err := budget.NewEngine(budget.Config{
		Providers: cfg.Providers, Accounts: cfg.Accounts, RetryReserve: cfg.Settings.Spec.Budget.RetryReserve.Fraction(),
	}, e.clock, logger)
	if err != nil {
		return &exitError{code: exitFailure, err: err}
	}
	exec := collect.New(collect.Options{Budget: engine, Clock: e.clock, Logger: logger, Retry: cfg.Settings.Spec.Retry})
	f.send = func(ctx context.Context, endpoint string, req *http.Request) (*http.Response, error) {
		return exec.SendAuth(ctx, f.scope, endpoint, req)
	}
	f.clock = e.clock

	var tr auth.TokenResponse
	switch {
	case o.device:
		tr, err = f.deviceLogin(ctx, e)
	default:
		var code, verifier string
		code, verifier, err = f.consent(ctx, e, o.listen)
		if err == nil {
			tr, err = f.exchange(ctx, code, verifier)
		}
	}
	if err != nil {
		return &exitError{code: exitFailure, err: err}
	}
	if tr.Refresh.IsZero() {
		return &exitError{code: exitFailure, err: errors.New("the token endpoint returned no refresh token: check the scopes the API needs for offline access")}
	}
	redactor.Add(tr.Refresh.Reveal())
	if err := writeSecretFile(o.secretFile, tr.Refresh.Reveal(), o.force); err != nil {
		return &exitError{code: exitFailure, err: err}
	}
	name := block.RefreshToken.Secret
	fmt.Fprintf(e.stdout, "\nWrote the refresh token of %s to %s (mode 0600).\n\n", account, o.secretFile)
	fmt.Fprintf(e.stdout, "Next steps:\n")
	fmt.Fprintf(e.stdout, "  1. Give it to the secret %s of the account, for example in its spec.secrets:\n", name)
	fmt.Fprintf(e.stdout, "       %s: { file: %s }\n", name, o.secretFile)
	fmt.Fprintf(e.stdout, "     or through an environment variable or a Kubernetes Secret.\n")
	fmt.Fprintf(e.stdout, "  2. Reload or restart tapyr run: the new token replaces the stored one.\n")
	fmt.Fprintf(e.stdout, "  3. Keep the file like the client secret, or delete it once the secret store holds it.\n")
	return nil
}

// loginAccount returns an account of the configuration and its provider,
// which must use oauth2_authorization_code.
func loginAccount(cfg *config.Config, name string) (*config.Account, *config.Provider, error) {
	for _, a := range cfg.Accounts {
		if string(a.Metadata.Name) != name {
			continue
		}
		for _, p := range cfg.Providers {
			if p.Metadata.Name != a.Spec.Provider {
				continue
			}
			if p.Spec.Auth.Type != config.AuthOAuth2AuthorizationCode || p.Spec.Auth.OAuth2AuthorizationCode == nil {
				return nil, nil, fmt.Errorf("account %s uses auth type %s, not oauth2_authorization_code: it has no consent to run", name, p.Spec.Auth.Type)
			}
			return a, p, nil
		}
		return nil, nil, fmt.Errorf("account %s: unknown provider %q", name, a.Spec.Provider)
	}
	return nil, nil, fmt.Errorf("unknown account %q", name)
}

// newLoginFlow renders the URLs of the auth block with the account params
// and resolves the client credentials (not the refresh token, which the
// login produces).
func newLoginFlow(a *config.Account, p *config.Provider, src config.SecretSource) (*loginFlow, error) {
	o := p.Spec.Auth.OAuth2AuthorizationCode
	f := &loginFlow{scope: auth.Scope{Provider: string(p.Metadata.Name), Account: string(a.Metadata.Name)}, block: o}
	params := map[string]string{}
	for k, v := range a.Spec.Params {
		params[k] = v
	}
	for _, u := range []struct {
		field string
		t     config.Template
		dst   **url.URL
	}{
		{"authorizeURL", o.AuthorizeURL, &f.authorizeURL},
		{"tokenURL", o.TokenURL, &f.tokenURL},
		{"deviceAuthorizationURL", o.DeviceAuthorizationURL, &f.deviceURL},
		{"redirectURL", o.RedirectURL, &f.redirectURL},
	} {
		if u.t == "" {
			continue
		}
		raw, err := config.RenderTemplate(u.field, string(u.t), params)
		if err != nil {
			return nil, err
		}
		parsed, err := url.Parse(raw)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return nil, fmt.Errorf("auth %s: %q is not an absolute http or https URL (check the account params)", u.field, raw)
		}
		*u.dst = parsed
	}
	resolve := func(field string, v config.Value, optional bool) (secret.Value, error) {
		if v.Secret == "" {
			return secret.New(v.Literal), nil
		}
		ref, ok := a.Spec.Secrets[v.Secret]
		if !ok {
			if optional {
				return secret.Value{}, nil
			}
			return secret.Value{}, fmt.Errorf("auth %s: account %s provides no secret %s", field, a.Metadata.Name, v.Secret)
		}
		val, err := secret.Resolve(ref, src)
		if err != nil {
			return secret.Value{}, fmt.Errorf("auth %s: secret %s: %w", field, v.Secret, err)
		}
		return val, nil
	}
	var err error
	if f.clientID, err = resolve("clientId", o.ClientID, false); err != nil {
		return nil, err
	}
	if o.ClientSecret != nil {
		if f.clientSecret, err = resolve("clientSecret", *o.ClientSecret, true); err != nil {
			return nil, err
		}
	}
	return f, nil
}

// randomToken returns n random bytes, base64url encoded.
func randomToken(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// consent prints the authorization URL and returns the code of the
// redirect, pasted or received by the listener, and the PKCE verifier.
func (f *loginFlow) consent(ctx context.Context, e *env, listen bool) (string, string, error) {
	state, verifier := randomToken(24), ""
	q := f.authorizeURL.Query()
	q.Set("response_type", "code")
	q.Set("client_id", f.clientID.Reveal())
	q.Set("redirect_uri", f.redirectURL.String())
	q.Set("state", state)
	if len(f.block.Scopes) > 0 {
		q.Set("scope", strings.Join(f.block.Scopes, f.block.ScopeSeparator))
	}
	if f.block.PKCE != "none" {
		verifier = randomToken(48)
		sum := sha256.Sum256([]byte(verifier))
		q.Set("code_challenge", base64.RawURLEncoding.EncodeToString(sum[:]))
		q.Set("code_challenge_method", "S256")
	}
	for k, v := range f.block.AuthorizeParams {
		q.Set(k, v)
	}
	u := *f.authorizeURL
	u.RawQuery = q.Encode()
	if listen {
		code, err := f.listenRedirect(ctx, e, u.String(), state)
		return code, verifier, err
	}
	fmt.Fprintf(e.stdout, "Open this URL in a browser, on any machine, and accept:\n\n  %s\n\n", u.String())
	fmt.Fprintf(e.stdout, "Paste the URL your browser was redirected to (or the code alone), then press Enter.\n")
	fmt.Fprintf(e.stdout, "The page itself may fail to load: only its address matters.\n")
	if e.stdin == nil {
		return "", "", errors.New("no standard input to read the redirect URL from: use --listen or --device")
	}
	line, err := bufio.NewReader(io.LimitReader(e.stdin, maxLoginResponse)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", "", fmt.Errorf("reading the redirect URL: %w", err)
	}
	code, err := codeFrom(line, state)
	return code, verifier, err
}

// codeFrom extracts the code of a pasted redirect URL, checking its state,
// or takes the input as the code itself.
func codeFrom(input, state string) (string, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return "", errors.New("no code pasted")
	}
	if !strings.Contains(input, "code=") && !strings.Contains(input, "error=") {
		return input, nil // the code alone
	}
	raw := input
	if i := strings.IndexByte(raw, '#'); i >= 0 {
		raw = raw[:i]
	}
	if i := strings.IndexByte(raw, '?'); i >= 0 {
		raw = raw[i+1:]
	}
	q, err := url.ParseQuery(raw)
	if err != nil {
		return "", fmt.Errorf("reading the pasted URL: %w", err)
	}
	return codeOf(q, state)
}

// codeOf checks the query of a redirect: an error, the state, the code.
func codeOf(q url.Values, state string) (string, error) {
	if e := q.Get("error"); e != "" {
		if d := q.Get("error_description"); d != "" {
			return "", fmt.Errorf("the authorization was refused: %s (%s)", e, d)
		}
		return "", fmt.Errorf("the authorization was refused: %s", e)
	}
	if q.Get("state") != state {
		return "", errors.New("the state of the redirect does not match this login: paste the URL of this login, not of an earlier one")
	}
	if q.Get("code") == "" {
		return "", errors.New("no code in the redirect URL")
	}
	return q.Get("code"), nil
}

// listenRedirect serves the redirect URL on this machine and waits for the
// browser.
func (f *loginFlow) listenRedirect(ctx context.Context, e *env, authorize, state string) (string, error) {
	r := f.redirectURL
	host := r.Hostname()
	if r.Scheme != "http" || (host != "127.0.0.1" && host != "localhost" && host != "::1") || r.Port() == "" {
		return "", fmt.Errorf("--listen needs a redirectURL on the loopback interface with a port, such as %s (got %s)", config.DefaultRedirectURL, r)
	}
	l, err := e.listen(ctx, "tcp", net.JoinHostPort(host, r.Port()))
	if err != nil {
		return "", fmt.Errorf("listening for the redirect: %w", err)
	}
	type result struct {
		code string
		err  error
	}
	results := make(chan result, 1)
	mux := http.NewServeMux()
	path := r.Path
	if path == "" {
		path = "/"
	}
	mux.HandleFunc(path, func(w http.ResponseWriter, req *http.Request) {
		code, err := codeOf(req.URL.Query(), state)
		msg := "Tapyr received the authorization. You can close this window."
		if err != nil {
			msg = "Tapyr could not use this authorization: " + err.Error()
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, "<!doctype html><title>Tapyr</title><p>%s</p>\n", html.EscapeString(msg))
		select {
		case results <- result{code, err}:
		default:
		}
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(l) }()
	defer func() { _ = srv.Close() }()
	fmt.Fprintf(e.stdout, "Open this URL in a browser on this machine, and accept:\n\n  %s\n\n", authorize)
	fmt.Fprintf(e.stdout, "Waiting for the redirect on %s...\n", r)
	select {
	case res := <-results:
		return res.code, res.err
	case <-ctx.Done():
		return "", ctx.Err()
	case <-e.clock.After(loginTimeout):
		return "", errors.New("no redirect received in 15 minutes")
	}
}

// exchange trades the code for tokens.
func (f *loginFlow) exchange(ctx context.Context, code, verifier string) (auth.TokenResponse, error) {
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {f.redirectURL.String()}}
	if verifier != "" {
		form.Set("code_verifier", verifier)
	}
	return f.token(ctx, form)
}

// token sends a form to the token endpoint, with tokenParams and the client
// authentication of the block.
func (f *loginFlow) token(ctx context.Context, form url.Values) (auth.TokenResponse, error) {
	for k, v := range f.block.TokenParams {
		form.Set(k, v)
	}
	status, body, err := f.post(ctx, f.tokenURL, form)
	if err != nil {
		return auth.TokenResponse{}, err
	}
	return auth.ParseTokenResponse("token endpoint "+f.tokenURL.Path, status, body)
}

func (f *loginFlow) post(ctx context.Context, u *url.URL, form url.Values) (int, []byte, error) {
	req, err := auth.NewTokenRequest(ctx, u, form, f.block.AuthStyle, f.clientID, f.clientSecret)
	if err != nil {
		return 0, nil, err
	}
	resp, err := f.send(ctx, u.Path, req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxLoginResponse))
	if err != nil {
		return 0, nil, fmt.Errorf("reading the %s response: %w", u.Path, err)
	}
	return resp.StatusCode, body, nil
}

// deviceLogin runs the device authorization grant: a code to enter at a
// URL, then the token endpoint polled until the person accepts.
func (f *loginFlow) deviceLogin(ctx context.Context, e *env) (auth.TokenResponse, error) {
	form := url.Values{}
	if len(f.block.Scopes) > 0 {
		form.Set("scope", strings.Join(f.block.Scopes, f.block.ScopeSeparator))
	}
	status, body, err := f.post(ctx, f.deviceURL, form)
	if err != nil {
		return auth.TokenResponse{}, err
	}
	var d struct {
		DeviceCode       string `json:"device_code"`
		UserCode         string `json:"user_code"`
		VerificationURI  string `json:"verification_uri"`
		VerificationFull string `json:"verification_uri_complete"`
		Interval         int    `json:"interval"`
	}
	if status != http.StatusOK || json.Unmarshal(body, &d) != nil || d.DeviceCode == "" || d.UserCode == "" || d.VerificationURI == "" {
		return auth.TokenResponse{}, fmt.Errorf("device authorization endpoint %s answered %d without a device code", f.deviceURL.Path, status)
	}
	fmt.Fprintf(e.stdout, "Open %s and enter the code %s\n", d.VerificationURI, d.UserCode)
	if d.VerificationFull != "" {
		fmt.Fprintf(e.stdout, "(or open %s directly)\n", d.VerificationFull)
	}
	fmt.Fprintf(e.stdout, "Waiting for the authorization...\n")
	interval := time.Duration(max(d.Interval, 5)) * time.Second
	ctx, cancel := context.WithTimeout(ctx, loginTimeout)
	defer cancel()
	for {
		if err := f.clock.Sleep(ctx, interval); err != nil {
			return auth.TokenResponse{}, fmt.Errorf("waiting for the authorization: %w", err)
		}
		form := url.Values{"grant_type": {deviceGrant}, "device_code": {d.DeviceCode}}
		for k, v := range f.block.TokenParams {
			form.Set(k, v)
		}
		status, body, err := f.post(ctx, f.tokenURL, form)
		if err != nil {
			return auth.TokenResponse{}, err
		}
		var oe struct {
			Error string `json:"error"`
		}
		if status == http.StatusBadRequest && json.Unmarshal(body, &oe) == nil {
			switch oe.Error {
			case "authorization_pending":
				continue
			case "slow_down":
				interval += 5 * time.Second
				continue
			case "access_denied", "expired_token":
				return auth.TokenResponse{}, fmt.Errorf("the authorization was not given: %s", oe.Error)
			}
		}
		return auth.ParseTokenResponse("token endpoint "+f.tokenURL.Path, status, body)
	}
}

// writeSecretFile writes a token to a file only its owner can read.
func writeSecretFile(path, token string, force bool) error {
	flag := os.O_WRONLY | os.O_CREATE | os.O_EXCL
	if force {
		flag = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	}
	file, err := os.OpenFile(path, flag, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%s already exists: use --force to overwrite it", path)
		}
		return fmt.Errorf("writing the refresh token: %w", err)
	}
	err = file.Chmod(0o600)
	if err == nil {
		_, err = io.WriteString(file, token+"\n")
	}
	if cerr := file.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("writing the refresh token: %w", err)
	}
	return nil
}
