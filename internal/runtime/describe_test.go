package runtime

import (
	"testing"

	"github.com/kentrow/tapyr/internal/config"
)

func TestTemplatePrefix(t *testing.T) {
	tests := []struct {
		names []string
		want  string
	}{
		{[]string{"example-sim-sites", "example-sim-networks", "example-sim-network-devices"}, "example-sim-"},
		{[]string{"meraki-organizations", "meraki-devices"}, "meraki-"},
		// The shared start is cut at its last hyphen.
		{[]string{"unifi-isp-metrics", "unifi-isp-metrics-hourly"}, "unifi-isp-"},
		{[]string{"orgs", "networks"}, ""},
		{[]string{"rms-devices"}, ""},
	}
	for _, tt := range tests {
		c := &compiler{templates: map[config.Name]*config.JobTemplate{}}
		for _, n := range tt.names {
			c.templates[config.Name(n)] = &config.JobTemplate{Spec: config.JobTemplateSpec{Provider: "p"}}
		}
		c.templates["other-provider-template"] = &config.JobTemplate{Spec: config.JobTemplateSpec{Provider: "other"}}
		if got := c.templatePrefix("p"); got != tt.want {
			t.Errorf("templatePrefix(%v) = %q, want %q", tt.names, got, tt.want)
		}
	}
}

func TestItemWords(t *testing.T) {
	tests := []struct{ template, prefix, want string }{
		{"example-sim-networks", "example-sim-", "networks"},
		{"example-sim-network-devices", "example-sim-", "network devices"},
		{"orgs", "", "orgs"},
	}
	for _, tt := range tests {
		if got := itemWords(tt.template, tt.prefix); got != tt.want {
			t.Errorf("itemWords(%q, %q) = %q, want %q", tt.template, tt.prefix, got, tt.want)
		}
	}
}
