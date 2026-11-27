package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kentrow/tapyr/internal/clock"
	"github.com/kentrow/tapyr/internal/provider"
	"github.com/kentrow/tapyr/internal/sim"
)

// loginProvider is a package with the oauth2_authorization_code auth type
// and its simulator: a client app/s3cr3t that rotates, the device flow.
const loginProvider = `apiVersion: tapyr/v1alpha1
kind: Provider
metadata: { name: athletes, version: 0.1.0 }
spec:
  baseURL: '{{ .params.baseURL | default "http://127.0.0.1:1/api" }}'
  params: [{ name: baseURL }, { name: redirectURL }]
  secrets: [{ name: clientId, required: true }, { name: clientSecret }, { name: refreshToken, required: true }]
  auth:
    type: oauth2_authorization_code
    oauth2AuthorizationCode:
      authorizeURL: '{{ .params.baseURL | default "http://127.0.0.1:1/api" }}/oauth/authorize'
      tokenURL: '{{ .params.baseURL | default "http://127.0.0.1:1/api" }}/oauth/token'
      deviceAuthorizationURL: '{{ .params.baseURL | default "http://127.0.0.1:1/api" }}/oauth/device'
      redirectURL: '{{ .params.redirectURL | default "http://127.0.0.1:8085/callback" }}'
      clientId: { secret: clientId }
      clientSecret: { secret: clientSecret }
      refreshToken: { secret: refreshToken }
      scopes: [read, activity:read_all]
      scopeSeparator: ","
  limits:
    - { name: rate, kind: rate, rate: 10/s, scope: { by: [account] }, source: { kind: assumed } }
`

const loginSimulation = `apiVersion: tapyr/v1alpha1
kind: Simulation
metadata: { name: athletes }
spec:
  basePath: /api
  auth:
    accept:
      authorizationCode:
        - { clientId: app, clientSecret: s3cr3t, rotate: true, redirectURLs: [REDIRECTS] }
  routes: [{ path: /me, response: { file: me.json } }]
`

// loginSetup writes the package and a configuration, and starts the
// simulator. It returns the configuration path, the provider directory,
// the simulator and its URL.
type loginSetup struct {
	config, providers, secretFile string
	server                        *sim.Server
	url                           string
	vars                          map[string]string
}

func newLoginSetup(t *testing.T, redirect string, extraAccounts string) *loginSetup {
	t.Helper()
	dir := t.TempDir()
	pkgDir := filepath.Join(dir, "providers", "athletes")
	files := map[string]string{
		"provider.yaml": loginProvider,
		"sim/sim.yaml":  strings.Replace(loginSimulation, "REDIRECTS", `"`+redirect+`"`, 1),
		"sim/me.json":   `{"id": 1}`,
	}
	for name, content := range files {
		p := filepath.Join(pkgDir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	loader, _, err := newLoader()
	if err != nil {
		t.Fatal(err)
	}
	pkgs, err := provider.FromPath(loader, pkgDir)
	if err != nil {
		t.Fatal(err)
	}
	s, err := sim.New(pkgs[0], sim.Options{})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	ls := &loginSetup{
		providers: filepath.Join(dir, "providers"), server: s, url: srv.URL, secretFile: filepath.Join(dir, "strava-refresh-token"),
		vars: map[string]string{"ATHLETES_CLIENT_ID": "app", "ATHLETES_CLIENT_SECRET": "s3cr3t"},
	}
	ls.config = filepath.Join(dir, "config.yaml")
	cfg := fmt.Sprintf(`apiVersion: tapyr/v1alpha1
kind: Account
metadata: { name: home }
spec:
  provider: athletes
  params: { baseURL: %q, redirectURL: %q }
  secrets:
    clientId: { env: ATHLETES_CLIENT_ID }
    clientSecret: { env: ATHLETES_CLIENT_SECRET }
    refreshToken: { file: %q }
%s`, srv.URL+"/api", redirect, ls.secretFile, extraAccounts)
	if err := os.WriteFile(ls.config, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return ls
}

// lineWriter collects the output and hands every line to a channel.
type lineWriter struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	lines chan string
	part  string
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf.Write(p)
	w.part += string(p)
	for {
		i := strings.IndexByte(w.part, '\n')
		if i < 0 {
			break
		}
		select {
		case w.lines <- w.part[:i]:
		default:
		}
		w.part = w.part[i+1:]
	}
	return len(p), nil
}

func (w *lineWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

type loginRun struct {
	code           int
	stdout, stderr string
}

// runLogin runs tapyr auth login; browser receives every output line and
// may write to the standard input of the command.
func runLogin(t *testing.T, ls *loginSetup, clk clock.Clock, browser func(line string, stdin io.Writer), args ...string) loginRun {
	t.Helper()
	stdinR, stdinW := io.Pipe()
	out := &lineWriter{lines: make(chan string, 64)}
	var stderr bytes.Buffer
	lookupEnv := func(k string) (string, bool) { v, ok := ls.vars[k]; return v, ok }
	e := &env{stdout: out, stderr: &stderr, stdin: stdinR, lookupEnv: lookupEnv, clock: clk, listen: (&net.ListenConfig{}).Listen}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	go func() {
		for {
			select {
			case line := <-out.lines:
				if browser != nil {
					browser(line, stdinW)
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	full := append([]string{"auth", "login", "home", "--config", ls.config, "--provider-path", ls.providers, "--secret-file", ls.secretFile}, args...)
	code := run(ctx, full, e)
	_ = stdinW.Close()
	res := loginRun{code: code, stdout: out.String(), stderr: stderr.String()}
	for _, leak := range []string{"sim-refresh", "sim-token", "sim-code", "s3cr3t"} {
		if strings.Contains(res.stdout+res.stderr, leak) {
			t.Errorf("the output carries %q:\n%s%s", leak, res.stdout, res.stderr)
		}
	}
	return res
}

// consentURL follows an authorize URL like a browser that consents,
// without following the redirect, and returns the redirect URL.
func consentURL(t *testing.T, authorize string) string {
	t.Helper()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, authorize, http.NoBody)
	resp, err := client.Do(req)
	if err != nil {
		t.Error(err)
		return ""
	}
	resp.Body.Close()
	return resp.Header.Get("Location")
}

// pasteBrowser consents when the authorize URL is printed and pastes the
// redirect URL, changed by edit.
func pasteBrowser(t *testing.T, edit func(redirect string) string) func(string, io.Writer) {
	return func(line string, stdin io.Writer) {
		u := strings.TrimSpace(line)
		if !strings.Contains(u, "/oauth/authorize?") {
			return
		}
		if parsed, err := url.Parse(u); err != nil || parsed.Query().Get("state") == "" {
			t.Errorf("authorize URL %s", u)
		}
		_, _ = io.WriteString(stdin, edit(consentURL(t, u))+"\n")
	}
}

// refreshWorks checks that the token written to the secret file is a
// refresh token the simulator accepts.
func refreshWorks(t *testing.T, ls *loginSetup) {
	t.Helper()
	data, err := os.ReadFile(ls.secretFile)
	if err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(ls.secretFile)
	if info.Mode().Perm() != 0o600 {
		t.Errorf("secret file mode %v, want 0600", info.Mode().Perm())
	}
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {strings.TrimSpace(string(data))}}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, ls.url+"/api/oauth/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth("app", "s3cr3t")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("the written refresh token is refused: %d", resp.StatusCode)
	}
}

func TestAuthLoginPaste(t *testing.T) {
	ls := newLoginSetup(t, "http://127.0.0.1:8085/callback", "")
	paste := pasteBrowser(t, func(r string) string { return r })
	var authorizeURL atomic.Value
	browser := func(line string, stdin io.Writer) {
		if strings.Contains(line, "/oauth/authorize?") {
			authorizeURL.Store(strings.TrimSpace(line))
		}
		paste(line, stdin)
	}
	res := runLogin(t, ls, clock.Real(), browser)
	u, _ := url.Parse(fmt.Sprint(authorizeURL.Load()))
	if q := u.Query(); q.Get("scope") != "read,activity:read_all" || q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" ||
		q.Get("client_id") != "app" || q.Get("response_type") != "code" {
		t.Errorf("authorize URL %s", u)
	}
	if res.code != 0 {
		t.Fatalf("exit %d\n%s%s", res.code, res.stdout, res.stderr)
	}
	for _, want := range []string{"Open this URL", "Paste", "Wrote the refresh token of home to " + ls.secretFile, "refreshToken: { file: " + ls.secretFile + " }", "Reload or restart tapyr run"} {
		if !strings.Contains(res.stdout, want) {
			t.Errorf("output lacks %q:\n%s", want, res.stdout)
		}
	}
	if st := ls.server.Stats(); st.Routes["POST /api/oauth/token"] != 1 {
		t.Errorf("token requests: %v", st.Routes)
	}
	refreshWorks(t, ls)
}

func TestAuthLoginPasteInputs(t *testing.T) {
	tests := []struct {
		name string
		edit func(redirect string) string
		ok   bool
		want string
	}{
		{name: "spaces around", edit: func(r string) string { return "  " + r + "  " }, ok: true},
		{name: "fragment", edit: func(r string) string { return r + "#_=_" }, ok: true},
		{name: "bare code", edit: func(r string) string { u, _ := url.Parse(r); return u.Query().Get("code") }, ok: true},
		{name: "state mismatch", edit: func(r string) string { return strings.Replace(r, "state=", "state=x", 1) }, want: "state"},
		{name: "access denied", edit: func(string) string {
			return "http://127.0.0.1:8085/callback?error=access_denied&error_description=The+user+said+no"
		}, want: "access_denied"},
		{name: "empty", edit: func(string) string { return "" }, want: "no code"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ls := newLoginSetup(t, "http://127.0.0.1:8085/callback", "")
			res := runLogin(t, ls, clock.Real(), pasteBrowser(t, tt.edit))
			if tt.ok {
				if res.code != 0 {
					t.Fatalf("exit %d\n%s%s", res.code, res.stdout, res.stderr)
				}
				refreshWorks(t, ls)
				return
			}
			if res.code == 0 || !strings.Contains(res.stderr, tt.want) {
				t.Fatalf("exit %d, want an error with %q\n%s%s", res.code, tt.want, res.stdout, res.stderr)
			}
			if _, err := os.Stat(ls.secretFile); !os.IsNotExist(err) {
				t.Errorf("a failed login wrote the secret file: %v", err)
			}
		})
	}
}

func TestAuthLoginListen(t *testing.T) {
	l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	redirect := fmt.Sprintf("http://127.0.0.1:%d/callback", port)
	ls := newLoginSetup(t, redirect, "")
	browser := func(line string, _ io.Writer) {
		u := strings.TrimSpace(line)
		if !strings.Contains(u, "/oauth/authorize?") {
			return
		}
		// A browser follows the redirect to the listener of the command.
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, u, http.NoBody)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Error(err)
			return
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if !strings.Contains(string(body), "close this window") {
			t.Errorf("listener page: %s", body)
		}
	}
	res := runLogin(t, ls, clock.Real(), browser, "--listen")
	if res.code != 0 {
		t.Fatalf("exit %d\n%s%s", res.code, res.stdout, res.stderr)
	}
	refreshWorks(t, ls)
}

func TestAuthLoginDevice(t *testing.T) {
	ls := newLoginSetup(t, "http://127.0.0.1:8085/callback", "")
	clk := clock.NewFake(time.Date(2026, time.November, 27, 9, 0, 0, 0, time.UTC))
	done := make(chan struct{})
	go driveClock(context.Background(), clk, done)
	res := runLogin(t, ls, clk, nil, "--device")
	close(done)
	if res.code != 0 {
		t.Fatalf("exit %d\n%s%s", res.code, res.stdout, res.stderr)
	}
	for _, want := range []string{"/oauth/authorize", "and enter the code sim-user-"} {
		if !strings.Contains(res.stdout, want) {
			t.Errorf("output lacks %q:\n%s", want, res.stdout)
		}
	}
	if st := ls.server.Stats(); st.Routes["POST /api/oauth/token"] != 2 || st.Routes["POST /api/oauth/device"] != 1 {
		t.Errorf("requests: %v", st.Routes)
	}
	refreshWorks(t, ls)
}

func TestAuthLoginSecretFile(t *testing.T) {
	ls := newLoginSetup(t, "http://127.0.0.1:8085/callback", "")
	if err := os.WriteFile(ls.secretFile, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := runLogin(t, ls, clock.Real(), pasteBrowser(t, func(r string) string { return r }))
	if res.code == 0 || !strings.Contains(res.stderr, "already exists") || !strings.Contains(res.stderr, "--force") {
		t.Fatalf("existing file: exit %d\n%s", res.code, res.stderr)
	}
	if data, _ := os.ReadFile(ls.secretFile); string(data) != "old" {
		t.Errorf("the existing file was changed: %q", data)
	}
	if st := ls.server.Stats(); st.Routes["POST /api/oauth/token"] != 0 {
		t.Errorf("the flow ran before checking the file: %v", st.Routes)
	}
	res = runLogin(t, ls, clock.Real(), pasteBrowser(t, func(r string) string { return r }), "--force")
	if res.code != 0 {
		t.Fatalf("--force: exit %d\n%s%s", res.code, res.stdout, res.stderr)
	}
	refreshWorks(t, ls)
}

func TestAuthLoginErrors(t *testing.T) {
	other := `---
apiVersion: tapyr/v1alpha1
kind: Account
metadata: { name: lab }
spec:
  provider: example-sim
  secrets: { apiKey: { env: K } }
`
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "unknown account", args: []string{"auth", "login", "nobody"}, want: `unknown account "nobody"`},
		{name: "other auth type", args: []string{"auth", "login", "lab"}, want: "uses auth type bearer, not oauth2_authorization_code"},
		{name: "no secret file", args: []string{"auth", "login", "home"}, want: "--secret-file is required"},
		{name: "listen and device", args: []string{"auth", "login", "home", "--listen", "--device"}, want: "either --listen or --device"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ls := newLoginSetup(t, "http://127.0.0.1:8085/callback", other)
			args := append(slicesClone(tt.args), "--config", ls.config, "--provider-path", ls.providers)
			if tt.name != "no secret file" {
				args = append(args, "--secret-file", ls.secretFile)
			}
			res := execute(t, ls.vars, args...)
			if res.code == 0 || !strings.Contains(res.stderr, tt.want) {
				t.Fatalf("exit %d, want %q\n%s%s", res.code, tt.want, res.stdout, res.stderr)
			}
		})
	}
}

func TestAuthLoginDeviceNotDeclared(t *testing.T) {
	ls := newLoginSetup(t, "http://127.0.0.1:8085/callback", "")
	pkgFile := filepath.Join(ls.providers, "athletes", "provider.yaml")
	data, _ := os.ReadFile(pkgFile)
	trimmed := strings.Replace(string(data), "      deviceAuthorizationURL: '{{ .params.baseURL | default \"http://127.0.0.1:1/api\" }}/oauth/device'\n", "", 1)
	if err := os.WriteFile(pkgFile, []byte(trimmed), 0o600); err != nil {
		t.Fatal(err)
	}
	res := execute(t, ls.vars, "auth", "login", "home", "--config", ls.config, "--provider-path", ls.providers, "--secret-file", ls.secretFile, "--device")
	if res.code == 0 || !strings.Contains(res.stderr, "declares no deviceAuthorizationURL") {
		t.Fatalf("exit %d\n%s", res.code, res.stderr)
	}
}

func slicesClone(s []string) []string { return append([]string(nil), s...) }
