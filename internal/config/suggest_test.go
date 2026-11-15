package config

import "testing"

func TestClosest(t *testing.T) {
	providers := []string{"aruba-central-classic", "cisco-thousandeyes", "example-sim", "juniper-mist", "meraki-dashboard", "teltonika-rms", "teltonika-rutos", "unifi-site-manager"}
	for _, tc := range []struct {
		name       string
		candidates []string
		want       string
	}{
		{"meraki", providers, "meraki-dashboard"},
		{"Meraki-Dashboard", providers, "meraki-dashboard"},
		{"juniper-mst", providers, "juniper-mist"},
		{"teltonika", providers, "teltonika-rms"}, // two packages contain it: the closest in edits, then by name
		{"intervall", []string{"account", "interval", "labels", "outputs", "priority", "template"}, "interval"},
		{"priorty", []string{"interval", "priority"}, "priority"},
		{"key", []string{"apiKey"}, "apiKey"},
		{"meraki-device", []string{"meraki-devices", "meraki-device-statuses", "meraki-networks"}, "meraki-devices"},
		{"acount", []string{"account", "amount"}, "account"},
		{"foo", providers, ""},
		{"x", []string{"y"}, ""},
		{"abc", []string{"abd"}, ""},
		{"", providers, ""},
	} {
		if got := closest(tc.name, tc.candidates); got != tc.want {
			t.Errorf("closest(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}
	if got := didYouMean("meraki", providers); got != ` (did you mean "meraki-dashboard"?)` {
		t.Errorf("didYouMean = %q", got)
	}
	if got := didYouMean("foo", providers); got != "" {
		t.Errorf("didYouMean = %q", got)
	}
}

func TestEditDistance(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"a", "", 1},
		{"", "abc", 3},
		{"kitten", "sitting", 3},
		{"priorty", "priority", 1},
		{"ab", "ba", 1},
		{"intervall", "interval", 1},
		{"météo", "meteo", 2},
	} {
		if got := editDistance(tc.a, tc.b); got != tc.want {
			t.Errorf("editDistance(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}
