package collector

import (
	"slices"
	"strings"

	"github.com/kentrow/tapyr/internal/runtime"
)

// scopeAccounts returns the accounts of a provider whose requests can count
// in a scope key of one of its limits (account=campus,target=north-01 or
// credential=<hash>): those with a job matching every account, target and
// credential dimension of the key. The other dimensions (instance,
// provider, endpoint, discovery variables) are shared by the accounts or
// known only per request, and narrow nothing: a scope keyed by them alone
// lists every account of the provider that has a job.
func scopeAccounts(provider, scope string, jobs []*runtime.Job) []string {
	want := map[string]string{}
	for _, part := range strings.Split(scope, ",") {
		dim, value, ok := strings.Cut(part, "=")
		if ok && (dim == "account" || dim == "target" || dim == "credential") {
			want[dim] = value
		}
	}
	out := []string{}
	for _, j := range jobs {
		if j.Provider != provider || slices.Contains(out, j.Account) {
			continue
		}
		if v, ok := want["account"]; ok && j.Account != v {
			continue
		}
		if v, ok := want["target"]; ok && j.Target != v {
			continue
		}
		if v, ok := want["credential"]; ok && j.Credential != v {
			continue
		}
		out = append(out, j.Account)
	}
	slices.Sort(out)
	return out
}
