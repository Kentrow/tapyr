//go:build e2e

package e2e

import (
	"regexp"
	"testing"
	"time"
)

// TestMerakiDashboard runs every template of the meraki-dashboard package
// against the real Dashboard API for one cycle: the organizations of the
// key, then one request per organization and template. A read-only key is
// enough, for example the one of a DevNet sandbox organization.
func TestMerakiDashboard(t *testing.T) {
	secrets := credentials(t, "TAPYR_E2E_MERAKI_API_KEY")
	config := `apiVersion: tapyr/v1alpha1
kind: Account
metadata: { name: meraki }
spec:
  provider: meraki-dashboard
  secrets: { apiKey: { env: TAPYR_E2E_MERAKI_API_KEY } }
`
	for _, template := range []string{
		"meraki-organizations", "meraki-networks", "meraki-devices", "meraki-device-availabilities",
		"meraki-appliance-uplink-statuses", "meraki-wireless-client-counts",
	} {
		config += `---
apiVersion: tapyr/v1alpha1
kind: Job
metadata: { name: ` + template + ` }
spec: { account: meraki, template: ` + template + `, interval: 1h }
`
	}
	addr := run(t, config, secrets)
	waitFor(t, "http://"+addr+"/metrics", 2*time.Minute,
		`(?m)^meraki_organization_info\{[^}]*organization_id="\d+"[^}]*\} 1$`,
		`(?m)^meraki_network_info\{[^}]*network_id="[^"]+"[^}]*\} 1$`,
		`(?m)^meraki_device_info\{[^}]*model="[^"]+"[^}]*serial="[^"]+"[^}]*\} 1$`,
		`(?m)^meraki_device_up\{[^}]*product_type="[^"]+",serial="[^"]+"\} [01]$`,
	)
	self := waitFor(t, "http://"+addr+"/metrics/self", time.Minute,
		`(?m)^tapyr_job_runs_total\{[^}]*job="meraki-device-availabilities"[^}]*result="success"[^}]*\} [1-9]`,
		`(?m)^tapyr_job_runs_total\{[^}]*job="meraki-devices"[^}]*result="success"[^}]*\} [1-9]`,
	)
	// An organization whose API access is disabled answers 403: report the
	// failures without failing, the other organizations must succeed.
	for _, m := range regexp.MustCompile(`(?m)^tapyr_job_runs_total\{[^}]*result="failed"[^}]*\} .*$`).FindAllString(self, -1) {
		t.Logf("failed runs: %s", m)
	}
}
