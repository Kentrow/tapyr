package runtime

import (
	"fmt"
	"maps"
	"slices"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestSplitCollected checks which jobs are collected (ADR 0013): orgs
// (inventory) feeds networks (both, network_up), which feeds devices
// (device_up); uptime stands alone (up).
func TestSplitCollected(t *testing.T) {
	base := `apiVersion: tapyr/v1alpha1
kind: Provider
metadata: { name: routers, version: 0.1.0 }
spec:
  baseURL: https://api.example.com
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
  extract: { inventory: { key: item.id, fields: { orgId: item.id } } }
---
apiVersion: tapyr/v1alpha1
kind: JobTemplate
metadata: { name: networks }
spec:
  provider: routers
  role: both
  defaultInterval: 30m
  forEach: { template: orgs, vars: { orgId: parent.orgId } }
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
  forEach: { template: networks, vars: { networkId: parent.networkId } }
  request: { path: "/orgs/{orgId}/networks/{networkId}/devices" }
  extract: { metrics: [{ name: device_up, value: "1.0" }, { name: device_clients, value: "1.0" }] }
  %s
---
apiVersion: tapyr/v1alpha1
kind: Job
metadata: { name: uptime }
spec:
  account: fleet
  interval: 5m
  request: { path: /uptime }
  extract: { metrics: [{ name: up, value: "1.0" }] }
%s`
	output := func(name, rules string) string {
		return "---\napiVersion: tapyr/v1alpha1\nkind: Output\nmetadata: { name: " + name + " }\nspec:\n  type: remote_write\n  remoteWrite: { url: http://127.0.0.1:8428/api/v1/write }\n" + rules
	}
	dropAll := "  processing: [{ action: drop, sourceLabels: [__name__], regex: \".*\" }]\n"
	for _, tt := range []struct {
		name, devicesOutputs, rest string
		want                       map[string]string // not collected job -> reason
	}{
		{name: "without outputs, every job is collected", rest: "", want: map[string]string{}},
		{name: "an output without rules keeps everything", rest: output("vm", ""), want: map[string]string{}},
		{
			name: "dropping some metrics of a job keeps it",
			rest: output("vm", "  processing: [{ action: drop, sourceLabels: [__name__], regex: device_up }]\n"),
			want: map[string]string{},
		},
		{
			name: "dropping every metric of a leaf job",
			rest: output("vm", "  processing: [{ action: drop, sourceLabels: [__name__], regex: \"device_.*\" }]\n"),
			want: map[string]string{"devices": ReasonNoOutputKeeps},
		},
		{
			name: "the parents follow their children",
			rest: output("vm", "  processing: [{ action: keep, sourceLabels: [__name__], regex: up }]\n"),
			want: map[string]string{"devices": ReasonNoOutputKeeps, "fleet.networks": ReasonNoOutputKeeps, "fleet.orgs": ReasonNoInventoryUse},
		},
		{
			name:           "a job sent only to an output that drops it",
			devicesOutputs: "outputs: [hosted]",
			rest:           output("local", "") + output("hosted", dropAll),
			want:           map[string]string{"devices": ReasonNoOutputKeeps},
		},
		{
			name: "every output drops everything",
			rest: output("vm", dropAll),
			want: map[string]string{
				"devices": ReasonNoOutputKeeps, "fleet.networks": ReasonNoOutputKeeps, "fleet.orgs": ReasonNoInventoryUse, "uptime": ReasonNoOutputKeeps,
			},
		},
		{
			name: "a rule on a label value cannot be decided",
			rest: output("vm", "  processing:\n    - { action: keep, sourceLabels: [site], regex: north }\n    - { action: drop, sourceLabels: [__name__], regex: \".*\" }\n"),
			want: map[string]string{},
		},
		{
			name: "skipUnconsumed false keeps every job",
			rest: output("vm", dropAll) + "---\napiVersion: tapyr/v1alpha1\nkind: Settings\nmetadata: { name: s }\nspec: { collection: { skipUnconsumed: false } }\n",
			want: map[string]string{},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := writeConfig(t, fmt.Sprintf(base, tt.devicesOutputs, tt.rest))
			g, err := Compile(load(t, path), withAuth(t, Options{}))
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]string{}
			for _, j := range g.NotCollected {
				got[j.Name] = j.NotCollected
			}
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("not collected (-want +got):\n%s", diff)
			}
			names := map[string]bool{}
			for _, j := range g.Jobs {
				names[j.Name] = true
				if j.NotCollected != "" {
					t.Errorf("collected job %s has reason %q", j.Name, j.NotCollected)
				}
			}
			if len(names)+len(got) != 4 {
				t.Errorf("collected %v and not collected %v: want the 4 jobs once", slices.Sorted(maps.Keys(names)), got)
			}
		})
	}
}
