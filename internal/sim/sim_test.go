package sim

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/kentrow/tapyr/internal/auth/plugins"
	"github.com/kentrow/tapyr/internal/config"
	"github.com/kentrow/tapyr/internal/provider"
)

func loader(t *testing.T) *config.Loader {
	t.Helper()
	l, err := config.NewLoader()
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func exampleSim(t *testing.T) *httptest.Server {
	t.Helper()
	pkgs, err := provider.Builtin(loader(t))
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(provider.Find(pkgs, "example-sim"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv
}

func get(t *testing.T, url, token string) (int, http.Header, any) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	var body any
	if err := json.Unmarshal(data, &body); err != nil {
		t.Fatalf("invalid JSON %q: %v", data, err)
	}
	return resp.StatusCode, resp.Header, body
}

func ids(t *testing.T, body any, field string) []string {
	t.Helper()
	list, ok := body.([]any)
	if !ok {
		t.Fatalf("body is not a list: %v", body)
	}
	out := make([]string, len(list))
	for i, item := range list {
		out[i] = item.(map[string]any)[field].(string)
	}
	return out
}

func TestExampleSim(t *testing.T) {
	srv := exampleSim(t)
	api := srv.URL + "/api/v1"

	t.Run("auth", func(t *testing.T) {
		for _, token := range []string{"", "wrong"} {
			status, _, body := get(t, api+"/sites", token)
			if status != http.StatusUnauthorized || body.(map[string]any)["error"] != "invalid credentials" {
				t.Errorf("token %q: %d %v", token, status, body)
			}
		}
	})

	t.Run("offset pagination from the template parameters", func(t *testing.T) {
		_, _, body := get(t, api+"/sites", "sim-api-key")
		if got := ids(t, body, "id"); strings.Join(got, ",") != "site-a,site-b" {
			t.Errorf("first page = %v", got)
		}
		_, _, body = get(t, api+"/sites?offset=2&limit=2", "sim-api-key")
		if got := ids(t, body, "id"); strings.Join(got, ",") != "site-c" {
			t.Errorf("second page = %v", got)
		}
		_, _, body = get(t, api+"/sites?offset=9&limit=2", "sim-api-key")
		if got := ids(t, body, "id"); len(got) != 0 {
			t.Errorf("beyond the end = %v", got)
		}
		if status, _, _ := get(t, api+"/sites?offset=x", "sim-api-key"); status != http.StatusBadRequest {
			t.Errorf("invalid offset status = %d", status)
		}
	})

	t.Run("link header pagination", func(t *testing.T) {
		var serials []string
		next := api + "/devices?perPage=2"
		for pages := 0; next != ""; pages++ {
			if pages > 5 {
				t.Fatal("pagination does not end")
			}
			_, header, body := get(t, next, "sim-api-key")
			if header.Get("X-RateLimit-Limit-Day") != "5000" {
				t.Errorf("route headers missing: %v", header)
			}
			serials = append(serials, ids(t, body, "serial")...)
			next = ""
			if link := header.Get("Link"); link != "" {
				next = strings.TrimPrefix(strings.Split(link, ">")[0], "<")
			}
		}
		if strings.Join(serials, ",") != "SIM-0001,SIM-0002,SIM-0003,SIM-0004,SIM-0005" {
			t.Errorf("serials = %v", serials)
		}
	})

	t.Run("unknown routes", func(t *testing.T) {
		for _, u := range []string{api + "/nope", srv.URL + "/sites", api + "/devices/extra"} {
			if status, _, _ := get(t, u, "sim-api-key"); status != http.StatusNotFound {
				t.Errorf("%s: status %d", u, status)
			}
		}
	})

	t.Run("status", func(t *testing.T) {
		_, _, body := get(t, srv.URL+"/_sim/status", "")
		m := body.(map[string]any)
		if m["provider"] != "example-sim" || len(m["routes"].([]any)) != 4 || m["requests"].(float64) == 0 || m["stats"] == nil {
			t.Errorf("status = %v", m)
		}
	})
}

const providerDoc = `apiVersion: tapyr/v1alpha1
kind: Provider
metadata: { name: acme, version: 0.1.0 }
spec:
  baseURL: https://api.acme.invalid
  auth: AUTH
  errors: { authFailed: { statuses: [403] } }
`

func pkg(t *testing.T, auth, sim string, files map[string]string) *config.Package {
	t.Helper()
	fsys := fstest.MapFS{
		"provider.yaml": &fstest.MapFile{Data: []byte(strings.Replace(providerDoc, "AUTH", auth, 1))},
		"sim/sim.yaml":  &fstest.MapFile{Data: []byte("apiVersion: tapyr/v1alpha1\nkind: Simulation\nmetadata: { name: acme }\nspec:\n" + sim)},
	}
	for name, content := range files {
		fsys[name] = &fstest.MapFile{Data: []byte(content)}
	}
	p, err := loader(t).LoadPackage(fsys, "acme", "acme", "test")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAuthTypesAndVariables(t *testing.T) {
	routes := `  auth: { accept: { header: [k1], basic: [{ username: u, password: p }] } }
  routes:
    - path: /orgs/{orgId}/devices
      response: { file: "fixtures/{orgId}.json", status: 201, headers: { X-Sim: "1" } }
`
	files := map[string]string{"sim/fixtures/o1.json": `{"org": "o1"}`}
	t.Run("header", func(t *testing.T) {
		s, err := New(pkg(t, `{ type: header, header: { name: X-Key, value: "x" } }`, routes, files), Options{})
		if err != nil {
			t.Fatal(err)
		}
		srv := httptest.NewServer(s.Handler())
		defer srv.Close()
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/orgs/o1/devices", http.NoBody)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("missing header: status %d, want the provider authFailed status 403", resp.StatusCode)
		}
		req.Header.Set("X-Key", "k1")
		resp, err = http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusCreated || resp.Header.Get("X-Sim") != "1" || !strings.Contains(string(data), `"org":"o1"`) {
			t.Errorf("status %d, headers %v, body %s", resp.StatusCode, resp.Header, data)
		}
		req2, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/orgs/o2/devices", http.NoBody)
		req2.Header.Set("X-Key", "k1")
		resp, err = http.DefaultClient.Do(req2)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("missing fixture: status %d", resp.StatusCode)
		}
	})
	t.Run("header with a format", func(t *testing.T) {
		s, err := New(pkg(t, `{ type: header, header: { name: Authorization, value: "x", format: "Token {value}" } }`, routes, files), Options{})
		if err != nil {
			t.Fatal(err)
		}
		srv := httptest.NewServer(s.Handler())
		defer srv.Close()
		for value, want := range map[string]int{"Token k1": http.StatusCreated, "k1": http.StatusForbidden, "Bearer k1": http.StatusForbidden} {
			req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/orgs/o1/devices", http.NoBody)
			req.Header.Set("Authorization", value)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != want {
				t.Errorf("Authorization %q: status %d, want %d", value, resp.StatusCode, want)
			}
		}
	})
	t.Run("basic", func(t *testing.T) {
		s, err := New(pkg(t, "{ type: basic, basic: { username: u, password: p } }", routes, files), Options{})
		if err != nil {
			t.Fatal(err)
		}
		srv := httptest.NewServer(s.Handler())
		defer srv.Close()
		for _, creds := range [][2]string{{"u", "p"}, {"u", "wrong"}} {
			req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/orgs/o1/devices", http.NoBody)
			req.SetBasicAuth(creds[0], creds[1])
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			want := http.StatusCreated
			if creds[1] == "wrong" {
				want = http.StatusForbidden
			}
			if resp.StatusCode != want {
				t.Errorf("%v: status %d, want %d", creds, resp.StatusCode, want)
			}
		}
	})
}

func TestNewErrors(t *testing.T) {
	tests := []struct {
		name     string
		auth     string
		sim      string
		files    map[string]string
		scenario *config.Scenario
		want     string
	}{
		{
			name: "missing fixture",
			auth: "{ type: none }",
			sim:  "  routes: [{ path: /a, response: { file: fixtures/a.json } }]\n",
			want: "route  /a: fixture fixtures/a.json: file does not exist",
		},
		{
			name:  "no pagination parameters",
			auth:  "{ type: none }",
			sim:   "  routes: [{ path: /a, pagination: offset, response: { file: a.json } }]\n",
			files: map[string]string{"sim/a.json": "{}"},
			want:  "no offset pagination declared for this path",
		},
		{
			name: "client-side pagination",
			auth: "{ type: none }",
			sim:  "  routes: [{ path: /a, pagination: expression, response: { file: a.json } }]\n",
			want: "pagination expression is not served by the simulator",
		},
		{
			name: "unknown auth plugin",
			auth: "{ type: plugin, plugin: { name: custom } }",
			sim:  "  routes: [{ path: /a, response: { file: a.json } }]\n",
			want: `unknown auth plugin "custom" (available: aruba_central_classic)`,
		},
		{
			name:     "scenario fixture of an unknown route",
			auth:     "{ type: none }",
			sim:      "  routes: [{ path: /a, response: { file: a.json } }]\n",
			files:    map[string]string{"sim/a.json": "[]"},
			scenario: fixturesScenario(map[string]string{"/b": "a.json"}),
			want:     "scenario s step 0: fixtures: no route /b",
		},
		{
			name:     "missing scenario fixture",
			auth:     "{ type: none }",
			sim:      "  routes: [{ path: /a, response: { file: a.json } }]\n",
			files:    map[string]string{"sim/a.json": "[]"},
			scenario: fixturesScenario(map[string]string{"/a": "b.json"}),
			want:     "scenario s step 0: fixture b.json: file does not exist",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New(pkg(t, tt.auth, tt.sim, tt.files), Options{Scenario: tt.scenario, Plugins: plugins.Builtin()})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("New() error = %v, want %q", err, tt.want)
			}
		})
	}
	// A package with an auth plugin needs the plugins.
	pluginPkg := pkg(t, "{ type: plugin, plugin: { name: custom } }", "  routes: [{ path: /a, response: { file: a.json } }]\n", map[string]string{"sim/a.json": "[]"})
	if _, err := New(pluginPkg, Options{}); err == nil || !strings.Contains(err.Error(), "Options.Plugins is required") {
		t.Errorf("New() without plugins = %v", err)
	}
	noSim := &config.Package{Provider: &config.Provider{Metadata: config.ProviderMeta{Name: "x"}}}
	if _, err := New(noSim, Options{}); err == nil || !strings.Contains(err.Error(), "provider x has no simulator") {
		t.Errorf("no simulator error = %v", err)
	}
}

func fixturesScenario(fixtures map[string]string) *config.Scenario {
	return &config.Scenario{
		Metadata: config.ObjectMeta{Name: "s"},
		Spec:     config.ScenarioSpec{Steps: []config.ScenarioStep{{Fixtures: fixtures}}},
	}
}
