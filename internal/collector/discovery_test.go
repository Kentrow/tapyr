package collector

import (
	"context"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kentrow/tapyr/internal/auth"
	"github.com/kentrow/tapyr/internal/auth/plugins"
	"github.com/kentrow/tapyr/internal/clock"
	"github.com/kentrow/tapyr/internal/config"
	"github.com/kentrow/tapyr/internal/mapping"
	"github.com/kentrow/tapyr/internal/provider"
	"github.com/kentrow/tapyr/internal/runtime"
	"github.com/kentrow/tapyr/internal/scheduler"
	"github.com/kentrow/tapyr/internal/state"
)

const discoveryConfig = `apiVersion: tapyr/v1alpha1
kind: Provider
metadata: { name: routers, version: 0.1.0 }
spec:
  baseURL: https://api.example.test
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
  secrets: { token: { env: TOKEN } }
---
apiVersion: tapyr/v1alpha1
kind: Job
metadata: { name: devices }
spec:
  account: fleet
  interval: 5m
  forEach: { template: networks, vars: { networkId: parent.networkId }, maxInstances: 2 }
  request: { path: "/orgs/{orgId}/networks/{networkId}/devices" }
  extract: { metrics: [{ name: device_up, value: "1.0" }] }
`

func compileDiscovery(t *testing.T) *runtime.Graph {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(discoveryConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := config.NewLoader()
	if err != nil {
		t.Fatal(err)
	}
	builtin, err := provider.Builtin(l)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := provider.LoadConfig(l, []string{path}, builtin)
	if err != nil {
		t.Fatal(err)
	}
	g, err := runtime.Compile(cfg, withAuth(t, runtime.Options{Secrets: &config.SecretSource{
		LookupEnv: func(string) (string, bool) { return "t", true },
		ReadFile:  func(p string) ([]byte, error) { return nil, &fs.PathError{Op: "open", Path: p, Err: fs.ErrNotExist} },
	}}))
	if err != nil {
		t.Fatal(err)
	}
	return g
}

type discoveryHarness struct {
	d       *discovery
	sched   *scheduler.Scheduler
	retired []string
}

func newDiscoveryHarness(t *testing.T, g *runtime.Graph, store state.Store, now time.Time) *discoveryHarness {
	t.Helper()
	h := &discoveryHarness{}
	h.d = newDiscovery(g.Jobs, store, func(instance string) { h.retired = append(h.retired, instance) }, slog.New(slog.DiscardHandler))
	h.sched = scheduler.New(nil, scheduler.Options{Clock: clock.NewFake(now), Runner: func(context.Context, scheduler.Run) {}})
	h.d.attach(context.Background(), h.sched, now)
	return h
}

func (h *discoveryHarness) instances() []string {
	h.d.mu.Lock()
	defer h.d.mu.Unlock()
	return slices.Sorted(maps.Keys(h.d.active))
}

func (h *discoveryHarness) job(t *testing.T, key string) *runtime.Job {
	t.Helper()
	h.d.mu.Lock()
	defer h.d.mu.Unlock()
	j, ok := h.d.active[key]
	if !ok {
		t.Fatalf("instance %s not active", key)
	}
	return j
}

func items(fields ...map[string]any) []mapping.InventoryItem {
	var out []mapping.InventoryItem
	for _, f := range fields {
		out = append(out, mapping.InventoryItem{Key: f["id"].(string), Fields: f})
	}
	return out
}

func TestDiscoveryReconcile(t *testing.T) {
	ctx := context.Background()
	g := compileDiscovery(t)
	store, err := state.OpenSQLite(ctx, filepath.Join(t.TempDir(), "tapyr.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, time.November, 2, 9, 0, 0, 0, time.UTC)
	h := newDiscoveryHarness(t, g, store, now)

	// Only the configured parent runs before any inventory.
	if diff := cmp.Diff([]string{"fleet.orgs"}, h.instances()); diff != "" {
		t.Fatalf("initial instances (-want +got):\n%s", diff)
	}
	orgs := h.job(t, "fleet.orgs")
	mustUpdate := func(parent *runtime.Job, it []mapping.InventoryItem, at time.Time) {
		t.Helper()
		if err := h.d.update(ctx, parent, it, at); err != nil {
			t.Fatal(err)
		}
	}
	mustUpdate(orgs, items(map[string]any{"id": "42", "orgId": "42", "kind": "prod"}, map[string]any{"id": "7", "orgId": "7", "kind": "lab"}), now)
	net := h.job(t, "fleet.networks[orgId=42]")
	mustUpdate(net, items(map[string]any{"id": "n1", "networkId": "n1"}, map[string]any{"id": "n2", "networkId": "n2"},
		map[string]any{"id": "n3", "networkId": "n3"}), now)

	// The lab org is filtered out and devices are capped at maxInstances.
	want := []string{"devices[networkId=n1,orgId=42]", "devices[networkId=n2,orgId=42]", "fleet.networks[orgId=42]", "fleet.orgs"}
	if diff := cmp.Diff(want, h.instances()); diff != "" {
		t.Fatalf("instances (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(map[string]int{"devices": 2, "fleet.networks": 1, "fleet.orgs": 1}, h.sched.Instances()); diff != "" {
		t.Errorf("scheduled instances (-want +got):\n%s", diff)
	}
	fanouts := map[string]int{}
	for _, e := range h.d.planEntries() {
		fanouts[scheduler.Group(e.Job)] = e.Fanout
	}
	if diff := cmp.Diff(map[string]int{"devices": 2, "fleet.networks": 1, "fleet.orgs": 1}, fanouts); diff != "" {
		t.Errorf("plan fan-outs (-want +got):\n%s", diff)
	}
	select {
	case <-h.d.changed:
	default:
		t.Error("no replan signaled")
	}

	// A restart starts the children from the cached inventory.
	h2 := newDiscoveryHarness(t, g, store, now.Add(time.Hour))
	if diff := cmp.Diff(want, h2.instances()); diff != "" {
		t.Errorf("instances after restart (-want +got):\n%s", diff)
	}

	// Removing a network removes its devices; removing the org removes the
	// whole subtree, and their series are retired.
	mustUpdate(net, items(map[string]any{"id": "n2", "networkId": "n2"}), now)
	if diff := cmp.Diff([]string{"devices[networkId=n1,orgId=42]"}, h.retired); diff != "" {
		t.Errorf("retired (-want +got):\n%s", diff)
	}
	h.retired = nil
	mustUpdate(orgs, nil, now)
	if diff := cmp.Diff([]string{"fleet.orgs"}, h.instances()); diff != "" {
		t.Errorf("instances after removal (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{"devices[networkId=n2,orgId=42]", "fleet.networks[orgId=42]"}, h.retired); diff != "" {
		t.Errorf("retired (-want +got):\n%s", diff)
	}
	if _, _, ok, _ := store.LoadInventory(ctx, "fleet.networks[orgId=42]"); ok {
		t.Error("the inventory of a removed instance is still cached")
	}
	// A run finishing after its instance was removed delivers nothing.
	if h.d.deliver("fleet.networks[orgId=42]", func() { t.Error("delivered for a removed instance") }) {
		t.Error("deliver() = true for a removed instance")
	}
	if err := h.d.update(ctx, net, items(map[string]any{"id": "n9", "networkId": "n9"}), now); err != nil || len(h.instances()) != 1 {
		t.Errorf("a removed parent created children: %v %v", h.instances(), err)
	}
}

func TestDiscoveryInventoryExpires(t *testing.T) {
	ctx := context.Background()
	g := compileDiscovery(t)
	store, err := state.OpenSQLite(ctx, filepath.Join(t.TempDir(), "tapyr.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, time.November, 2, 9, 0, 0, 0, time.UTC)
	h := newDiscoveryHarness(t, g, store, now)
	if err := h.d.update(ctx, h.job(t, "fleet.orgs"), items(map[string]any{"id": "42", "orgId": "42", "kind": "prod"}), now); err != nil {
		t.Fatal(err)
	}

	// The TTL defaults to 3x the effective interval of the parent (1h).
	h.d.expire(ctx, now.Add(3*time.Hour))
	if len(h.instances()) != 2 {
		t.Fatalf("expired within the TTL: %v", h.instances())
	}
	h.d.expire(ctx, now.Add(3*time.Hour+time.Second))
	if diff := cmp.Diff([]string{"fleet.orgs"}, h.instances()); diff != "" {
		t.Errorf("instances after expiry (-want +got):\n%s", diff)
	}
	// A stale cache does not start children after a restart.
	h2 := newDiscoveryHarness(t, g, store, now.Add(4*time.Hour))
	if diff := cmp.Diff([]string{"fleet.orgs"}, h2.instances()); diff != "" {
		t.Errorf("instances after restart (-want +got):\n%s", diff)
	}
}

// withAuth gives the options a token manager with the built-in plugins.
func withAuth(t *testing.T, o runtime.Options) runtime.Options {
	t.Helper()
	if o.Auth == nil {
		m, err := auth.NewManager(auth.ManagerOptions{Clock: clock.Real(), Plugins: plugins.Builtin()})
		if err != nil {
			t.Fatal(err)
		}
		o.Auth = m
	}
	return o
}
