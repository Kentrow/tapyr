package collector

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/kentrow/tapyr/internal/runtime"
)

func TestScopeAccounts(t *testing.T) {
	jobs := []*runtime.Job{
		{Provider: "rms", Account: "fleet-a", Credential: "c1"},
		{Provider: "rms", Account: "fleet-a", Credential: "c1"},
		{Provider: "rms", Account: "fleet-b", Credential: "c1"},
		{Provider: "rms", Account: "fleet-c", Credential: "c2"},
		{Provider: "rutos", Account: "branch", Target: "north-01"},
		{Provider: "rutos", Account: "branch", Target: "south-01"},
		{Provider: "rutos", Account: "hq", Target: "south-01"},
		{Provider: "meraki", Account: "campus"},
	}
	for _, tc := range []struct {
		provider, scope string
		want            []string
	}{
		{"rms", "account=fleet-c", []string{"fleet-c"}},
		{"rms", "credential=c1", []string{"fleet-a", "fleet-b"}},
		{"rms", "credential=unknown", []string{}},
		{"rutos", "target=north-01", []string{"branch"}},
		{"rutos", "target=south-01", []string{"branch", "hq"}},
		{"rutos", "account=hq,target=south-01", []string{"hq"}},
		{"rms", "instance", []string{"fleet-a", "fleet-b", "fleet-c"}},
		{"meraki", "organizationId=42", []string{"campus"}},
		{"meraki", "provider=meraki,endpoint=/organizations", []string{"campus"}},
	} {
		if diff := cmp.Diff(tc.want, scopeAccounts(tc.provider, tc.scope, jobs)); diff != "" {
			t.Errorf("%s %s (-want +got):\n%s", tc.provider, tc.scope, diff)
		}
	}
}
