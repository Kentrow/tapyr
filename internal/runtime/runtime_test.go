package runtime

import (
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kentrow/tapyr/internal/auth"
	"github.com/kentrow/tapyr/internal/auth/plugins"
	"github.com/kentrow/tapyr/internal/config"
	"github.com/kentrow/tapyr/internal/mapping"
	"github.com/kentrow/tapyr/internal/provider"
)

func load(t *testing.T, paths ...string) *config.Config {
	t.Helper()
	l, err := config.NewLoader()
	if err != nil {
		t.Fatal(err)
	}
	builtin, err := provider.Builtin(l)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := provider.LoadConfig(l, paths, builtin)
	if err != nil {
		t.Fatalf("LoadConfig() = %v", err)
	}
	return cfg
}

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func secrets(env map[string]string) *config.SecretSource {
	return &config.SecretSource{
		LookupEnv: func(k string) (string, bool) { v, ok := env[k]; return v, ok },
		ReadFile: func(p string) ([]byte, error) {
			return nil, &fs.PathError{Op: "open", Path: p, Err: fs.ErrNotExist}
		},
	}
}

var examples = filepath.Join("..", "..", "examples", "config")

func TestCompileExamples(t *testing.T) {
	cfg := load(t, examples)
	g, err := Compile(cfg, withAuth(t, Options{}))
	if err != nil {
		t.Fatal(err)
	}
	// demo-devices, demo-network-devices, demo-sites and the implicit
	// demo.example-sim-networks.
	if len(g.Jobs) != 4 {
		t.Fatalf("jobs = %d", len(g.Jobs))
	}
	devices := g.Jobs[0]
	if devices.Name != "demo-devices" || devices.Interval != 2*time.Minute || devices.Priority != config.PriorityCritical {
		t.Errorf("job = %s %v %s", devices.Name, devices.Interval, devices.Priority)
	}
	if got := devices.BaseURL.String(); got != "http://127.0.0.1:8081/api/v1" {
		t.Errorf("base URL = %s", got)
	}
	if devices.Request.Method != http.MethodGet || devices.Request.Path != "/devices" || devices.Request.Timeout != 10*time.Second {
		t.Errorf("request = %+v", devices.Request)
	}
	if diff := cmp.Diff(map[string]string{"Accept": "application/json"}, devices.Request.Headers); diff != "" {
		t.Errorf("headers: %s", diff)
	}
	if p := devices.Pagination; p.Type != config.PaginationLinkHeader || p.LinkHeader.PageSize != 2 || p.MaxPages != 100 {
		t.Errorf("pagination = %+v", p)
	}
	byName := map[string]*Job{}
	for _, j := range g.Jobs {
		byName[j.Name] = j
	}
	if sites := byName["demo-sites"]; sites.Pagination.Type != config.PaginationOffset || sites.Pagination.Offset.PageSize != 2 {
		t.Errorf("sites pagination = %+v", sites.Pagination)
	}
	if nd := byName["demo-network-devices"]; nd.ForEach == nil || nd.ForEach.ParentJob != "demo.example-sim-networks" ||
		nd.Pagination.Type != config.PaginationCursor {
		t.Errorf("network devices = %+v", nd)
	}
	if nets := byName["demo.example-sim-networks"]; !nets.Implicit || nets.ForEach.ParentJob != "demo-sites" ||
		nets.Items != "networks" || !slices.Equal(nets.DiscoveryFor, []string{"demo-network-devices"}) {
		t.Errorf("implicit networks = %+v", nets)
	}
	if sites := byName["demo-sites"]; sites.Items != "" || sites.DiscoveryFor != nil {
		t.Errorf("a declared job has no discovery description: %q %v", sites.Items, sites.DiscoveryFor)
	}
	if devices.Auth != nil {
		t.Error("auth must not be built without secrets")
	}
	if diff := cmp.Diff([]map[string]string{{"tenant": "demo"}, nil, nil}, devices.Labels); diff != "" {
		t.Errorf("labels: %s", diff)
	}

	g, err = Compile(cfg, withAuth(t, Options{Secrets: secrets(map[string]string{"TAPYR_EXAMPLE_API_KEY": "sim-api-key"})}))
	if err != nil {
		t.Fatal(err)
	}
	if g.Jobs[0].Auth == nil {
		t.Error("auth missing with resolved secrets")
	}

	_, err = Compile(cfg, withAuth(t, Options{Secrets: secrets(nil)}))
	if err == nil || !strings.Contains(err.Error(), "spec.secrets.apiKey: environment variable TAPYR_EXAMPLE_API_KEY is not set") {
		t.Errorf("missing secret error = %v", err)
	}
}

func TestCompileErrors(t *testing.T) {
	path := writeConfig(t, `apiVersion: tapyr/v1alpha1
kind: Account
metadata: { name: bad-url }
spec:
  provider: example-sim
  params: { baseURL: "ftp://files" }
  secrets: { apiKey: { env: K } }
---
apiVersion: tapyr/v1alpha1
kind: Account
metadata: { name: demo }
spec: { provider: example-sim, secrets: { apiKey: { env: K } } }
---
apiVersion: tapyr/v1alpha1
kind: Job
metadata: { name: inline }
spec:
  account: demo
  interval: 1m
  request:
    path: /devices
    query: { since: { expr: "item.x" } }
  pagination:
    type: offset
    offset: { offsetParam: o, limitParam: l, pageSize: 10, stop: "size(response.body)" }
  extract:
    items: response.body
    metrics:
      - name: up
        value: "itme.status"
`)
	_, err := Compile(load(t, path), withAuth(t, Options{}))
	var diags config.Diagnostics
	if !errors.As(err, &diags) {
		t.Fatalf("Compile() error = %v", err)
	}
	got := strings.ReplaceAll(diags.Error(), path, "config.yaml")
	want := strings.Join([]string{
		`config.yaml:5:13: spec.provider: provider example-sim, baseURL: "ftp://files" is not an absolute http or https URL (check the account params)`,
		`config.yaml:22:14: spec.request.query.since: invalid expression: undeclared reference to 'item' (at column 1)`,
		`config.yaml:25:66: spec.pagination.offset.stop: expression must return a bool, not int`,
		`config.yaml:30:16: spec.extract.metrics[0].value: invalid expression: undeclared reference to 'itme' (did you mean 'item'?) (at column 1)`,
	}, "\n")
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("diagnostics (-want +got):\n%s", diff)
	}
}

// authCodeProvider is a provider with the oauth2_authorization_code auth
// type, for the compile tests of its account rules.
const authCodeProvider = `apiVersion: tapyr/v1alpha1
kind: Provider
metadata: { name: athletes, version: 0.1.0 }
spec:
  baseURL: https://api.example.invalid
  secrets: [{ name: clientId, required: true }, { name: clientSecret }, { name: refreshToken, required: true }]
  auth:
    type: oauth2_authorization_code
    oauth2AuthorizationCode:
      authorizeURL: https://auth.example.invalid/authorize
      tokenURL: https://auth.example.invalid/token
      clientId: { secret: clientId }
      clientSecret: { secret: clientSecret }
      refreshToken: { secret: refreshToken }
---
apiVersion: tapyr/v1alpha1
kind: JobTemplate
metadata: { name: activities }
spec:
  provider: athletes
  defaultInterval: 1h
  request: { path: /activities }
  extract: { metrics: [{ name: activities, value: "1.0" }] }
`

func TestCompileAuthCodeTargetSecrets(t *testing.T) {
	// The token belongs to the account: a target cannot bring its own
	// client or refresh token (ADR 0016).
	path := writeConfig(t, authCodeProvider+`---
apiVersion: tapyr/v1alpha1
kind: Account
metadata: { name: home }
spec:
  provider: athletes
  secrets: { clientId: { env: ID }, refreshToken: { env: RT } }
  targets:
    - { name: alice }
    - { name: bob, secrets: { refreshToken: { env: RT_BOB }, clientSecret: { env: SECRET_BOB } } }
---
apiVersion: tapyr/v1alpha1
kind: Job
metadata: { name: activities }
spec: { account: home, template: activities }
`)
	_, err := Compile(load(t, path), withAuth(t, Options{}))
	var diags config.Diagnostics
	if !errors.As(err, &diags) {
		t.Fatalf("Compile() error = %v", err)
	}
	got := strings.ReplaceAll(diags.Error(), path, "config.yaml")
	for _, want := range []string{
		"spec.targets[1].secrets.clientSecret: secret clientSecret of provider athletes belongs to the account (oauth2_authorization_code): a target cannot override it; use one account per person",
		"spec.targets[1].secrets.refreshToken: secret refreshToken of provider athletes belongs to the account (oauth2_authorization_code): a target cannot override it; use one account per person",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("diagnostics lack %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "targets[0]") {
		t.Errorf("target without secrets reported:\n%s", got)
	}
}

func TestCompileAuthCodeNotImplemented(t *testing.T) {
	path := writeConfig(t, authCodeProvider+`---
apiVersion: tapyr/v1alpha1
kind: Account
metadata: { name: home }
spec:
  provider: athletes
  secrets: { clientId: { env: ID }, refreshToken: { env: RT } }
---
apiVersion: tapyr/v1alpha1
kind: Job
metadata: { name: activities }
spec: { account: home, template: activities }
`)
	_, err := Compile(load(t, path), withAuth(t, Options{Secrets: secrets(map[string]string{"ID": "id", "RT": "rt"})}))
	if err == nil || !strings.Contains(err.Error(), "auth type not supported yet: oauth2_authorization_code") {
		t.Fatalf("Compile() error = %v", err)
	}
}

func TestTargetsAndDiscovery(t *testing.T) {
	path := writeConfig(t, `apiVersion: tapyr/v1alpha1
kind: Provider
metadata: { name: routers, version: 0.1.0 }
spec:
  baseURL: "{{ .params.baseURL }}"
  params: [{ name: baseURL, required: true }, { name: region }]
  secrets: [{ name: token, required: true }]
  auth: { type: bearer, bearer: { token: { secret: token } } }
---
apiVersion: tapyr/v1alpha1
kind: JobTemplate
metadata: { name: orgs }
spec:
  provider: routers
  role: inventory
  defaultInterval: 1h
  request: { path: /orgs }
  extract: { inventory: { key: item.id, fields: { orgId: item.id, kind: item.kind } } }
---
apiVersion: tapyr/v1alpha1
kind: JobTemplate
metadata: { name: networks }
spec:
  provider: routers
  role: both
  defaultInterval: 30m
  forEach: { template: orgs, vars: { orgId: parent.orgId }, filter: "parent.kind != 'lab'" }
  request: { path: "/orgs/{orgId}/networks" }
  extract:
    inventory: { key: item.id, fields: { networkId: item.id } }
    metrics: [{ name: network_up, value: "1.0" }]
---
apiVersion: tapyr/v1alpha1
kind: Account
metadata: { name: fleet }
spec:
  provider: routers
  params: { region: eu }
  secrets: { token: { env: TOKEN } }
  labels: { tenant: acme }
  targets:
    - { name: north, params: { baseURL: "https://10.0.0.1" }, labels: { site: north } }
    - { name: south, params: { baseURL: "https://10.0.0.2", region: fr }, secrets: { token: { env: SOUTH_TOKEN } } }
---
apiVersion: tapyr/v1alpha1
kind: Job
metadata: { name: devices }
spec:
  account: fleet
  interval: 5m
  forEach: { template: networks, vars: { networkId: parent.networkId } }
  request: { path: "/orgs/{orgId}/networks/{networkId}/devices" }
  extract: { metrics: [{ name: device_up, value: "1.0" }] }
`)
	g, err := Compile(load(t, path), withAuth(t, Options{Secrets: secrets(map[string]string{"TOKEN": "t1", "SOUTH_TOKEN": "t2"}), CredentialSalt: []byte("salt")}))
	if err != nil {
		t.Fatal(err)
	}
	byInstance := map[string]*Job{}
	for _, j := range g.Jobs {
		byInstance[j.Instance] = j
	}
	want := []string{
		"devices[target=north]", "devices[target=south]", "fleet.networks[target=north]", "fleet.networks[target=south]",
		"fleet.orgs[target=north]", "fleet.orgs[target=south]",
	}
	var got []string
	for _, j := range g.Jobs {
		got = append(got, j.Instance)
	}
	slices.Sort(got)
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("instances (-want +got):\n%s", diff)
	}
	north, south := byInstance["devices[target=north]"], byInstance["devices[target=south]"]
	if north.BaseURL.String() != "https://10.0.0.1" || south.BaseURL.String() != "https://10.0.0.2" {
		t.Errorf("base URLs: %s %s", north.BaseURL, south.BaseURL)
	}
	if north.Vars["region"] != "eu" || south.Vars["region"] != "fr" || north.Credential == south.Credential {
		t.Errorf("target params or credentials: %v %v", north.Vars, south.Vars)
	}
	if diff := cmp.Diff([]map[string]string{{"tenant": "acme"}, {"site": "north"}, nil}, north.Labels); diff != "" {
		t.Errorf("labels: %s", diff)
	}
	if north.TargetInfo["name"] != "north" {
		t.Errorf("target info = %v", north.TargetInfo)
	}
	orgs := byInstance["fleet.orgs[target=north]"]
	networks := byInstance["fleet.networks[target=north]"]
	if !orgs.Implicit || orgs.Priority != config.PriorityLow || orgs.Interval != time.Hour || orgs.ForEach != nil {
		t.Errorf("implicit parent = %+v", orgs)
	}
	if networks.ForEach.ParentJob != "fleet.orgs" || north.ForEach.ParentJob != "fleet.networks" || networks.Role != config.RoleBoth {
		t.Errorf("parents: %q %q", networks.ForEach.ParentJob, north.ForEach.ParentJob)
	}
	// Two implicit levels above devices, described alike on every target.
	for _, inst := range []string{"fleet.orgs[target=north]", "fleet.orgs[target=south]", "fleet.networks[target=south]"} {
		j := byInstance[inst]
		if want := strings.TrimSuffix(strings.TrimPrefix(inst, "fleet."), "[target=north]"); j.Items != strings.TrimSuffix(want, "[target=south]") ||
			!slices.Equal(j.DiscoveryFor, []string{"devices"}) {
			t.Errorf("%s: items %q, for %v", inst, j.Items, j.DiscoveryFor)
		}
	}

	// Children inherit the discovery variables of their parent instance.
	net, ok, err := networks.Child(orgs, mapping.InventoryItem{Key: "42", Fields: map[string]any{"orgId": int64(42), "kind": "prod"}}, time.Now())
	if err != nil || !ok || net.Instance != "fleet.networks[target=north,orgId=42]" || net.Vars["orgId"] != "42" || net.Vars["region"] != "eu" {
		t.Fatalf("network instance = %+v, %v, %v", net, ok, err)
	}
	if _, ok, _ := networks.Child(orgs, mapping.InventoryItem{Key: "7", Fields: map[string]any{"orgId": "7", "kind": "lab"}}, time.Now()); ok {
		t.Error("forEach.filter did not exclude the item")
	}
	dev, ok, err := north.Child(net, mapping.InventoryItem{Key: "n1", Fields: map[string]any{"networkId": "n1"}}, time.Now())
	if err != nil || !ok || dev.Instance != "devices[target=north,networkId=n1,orgId=42]" || dev.Vars["orgId"] != "42" {
		t.Fatalf("device instance = %+v, %v, %v", dev, ok, err)
	}
	if _, _, err := north.Child(net, mapping.InventoryItem{Key: "x", Fields: map[string]any{}}, time.Now()); err == nil {
		t.Error("missing parent field accepted")
	}
}

func TestOutputCredentials(t *testing.T) {
	path := writeConfig(t, `apiVersion: tapyr/v1alpha1
kind: Output
metadata: { name: vm }
spec:
  type: remote_write
  remoteWrite:
    url: http://vm:8428/api/v1/write
    headers: { X-Scope-OrgID: tenant, X-Token: { env: VM_TOKEN } }
    basicAuth: { username: { env: VM_USER }, password: { env: VM_PASSWORD } }
---
apiVersion: tapyr/v1alpha1
kind: Output
metadata: { name: collector }
spec: { type: otlp, otlp: { endpoint: "http://otel:4318", headers: { api-key: { env: OTEL_KEY } } } }
---
apiVersion: tapyr/v1alpha1
kind: Output
metadata: { name: prom }
spec: { type: prometheus }
`)
	vars := map[string]string{"VM_TOKEN": "vm-token", "VM_USER": "tapyr", "VM_PASSWORD": "vm-password", "OTEL_KEY": "otel-key"}
	src := &config.SecretSource{LookupEnv: func(k string) (string, bool) { v, ok := vars[k]; return v, ok }}
	g, err := Compile(load(t, path), withAuth(t, Options{Secrets: src}))
	if err != nil {
		t.Fatal(err)
	}
	vm := g.OutputCredentials["vm"]
	if vm == nil || vm.Headers["X-Scope-OrgID"].Reveal() != "tenant" || vm.Headers["X-Token"].Reveal() != "vm-token" ||
		vm.Username.Reveal() != "tapyr" || vm.Password.Reveal() != "vm-password" {
		t.Errorf("vm credentials = %+v", vm)
	}
	if c := g.OutputCredentials["collector"]; c == nil || c.Headers["api-key"].Reveal() != "otel-key" || !c.Username.IsZero() {
		t.Errorf("collector credentials = %+v", c)
	}
	if _, ok := g.OutputCredentials["prom"]; ok {
		t.Error("credentials for the pull output")
	}

	delete(vars, "VM_PASSWORD")
	_, err = Compile(load(t, path), withAuth(t, Options{Secrets: src}))
	if err == nil || !strings.Contains(err.Error(), "spec.remoteWrite.basicAuth.password: environment variable VM_PASSWORD is not set") {
		t.Errorf("missing secret error = %v", err)
	}
}

func TestOTLPEndpoint(t *testing.T) {
	for endpoint, wantErr := range map[string]bool{
		"http://otel:4318": false, "https://otel:4317": false, "otel:4317": true, "ftp://otel": true,
	} {
		path := writeConfig(t, fmt.Sprintf(`apiVersion: tapyr/v1alpha1
kind: Output
metadata: { name: collector }
spec: { type: otlp, otlp: { endpoint: %q } }
`, endpoint))
		_, err := Compile(load(t, path), withAuth(t, Options{}))
		if (err != nil) != wantErr || (err != nil && !strings.Contains(err.Error(), "spec.otlp.endpoint: endpoint")) {
			t.Errorf("%s: %v", endpoint, err)
		}
	}
}

func TestAccountTLS(t *testing.T) {
	dir := t.TempDir()
	notPEM := filepath.Join(dir, "not.pem")
	if err := os.WriteFile(notPEM, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	account := func(tls string) string {
		return writeConfig(t, fmt.Sprintf(`apiVersion: tapyr/v1alpha1
kind: Account
metadata: { name: lab }
spec:
  provider: example-sim
  secrets: { apiKey: { env: KEY } }
  %s
`, tls))
	}
	for _, tc := range []struct {
		name, tls, err string
		insecure       bool
		none           bool
	}{
		{name: "none", none: true},
		{name: "empty", tls: "tls: {}", none: true},
		{name: "insecure", tls: "tls: { insecureSkipVerify: true }", insecure: true},
		{name: "missing", tls: fmt.Sprintf("tls: { caFile: %q }", filepath.Join(dir, "missing.pem")), err: "spec.tls.caFile: reading the CA bundle"},
		{name: "not PEM", tls: fmt.Sprintf("tls: { caFile: %q }", notPEM), err: "holds no PEM certificate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, err := Compile(load(t, account(tc.tls)), withAuth(t, Options{}))
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("Compile() = %v, want %q", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got := g.TLS["lab"]
			if tc.none {
				if got != nil {
					t.Fatalf("TLS = %+v, want none", got)
				}
				return
			}
			if got == nil || got.Insecure != tc.insecure || got.Config.InsecureSkipVerify != tc.insecure || got.Fingerprint == "" {
				t.Fatalf("TLS = %+v", got)
			}
		})
	}
}

// withAuth gives the options a token manager with the built-in plugins,
// as cmd/tapyr does.
func withAuth(t *testing.T, o Options) Options {
	t.Helper()
	if o.Auth == nil {
		m, err := auth.NewManager(auth.ManagerOptions{Plugins: plugins.Builtin()})
		if err != nil {
			t.Fatal(err)
		}
		o.Auth = m
	}
	return o
}
