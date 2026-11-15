package collector

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/kentrow/tapyr/internal/config"
)

func TestWarnRestartSettings(t *testing.T) {
	base := func() *config.SettingsSpec {
		s := &config.SettingsSpec{}
		s.Server.Listen = "127.0.0.1:9790"
		s.State.Path = "./data/tapyr.db"
		s.Log.Level = "info"
		return s
	}
	enabled := true
	tests := []struct {
		name   string
		change func(s *config.SettingsSpec)
		want   string // the settings warned about, "" for none
	}{
		{"nothing", func(*config.SettingsSpec) {}, ""},
		// The live debug is applied on reload.
		{"debug only", func(s *config.SettingsSpec) { s.Server.Debug = config.DebugSettings{Enabled: true, MaxBodyBytes: 1024} }, ""},
		{"listen", func(s *config.SettingsSpec) { s.Server.Listen = "0.0.0.0:9790" }, "server"},
		{"ui", func(s *config.SettingsSpec) { s.Server.UI.Enabled = &enabled }, "server"},
		{"listen and debug", func(s *config.SettingsSpec) {
			s.Server.Listen = "0.0.0.0:9790"
			s.Server.Debug.Enabled = true
		}, "server"},
		{"state path", func(s *config.SettingsSpec) { s.State.Path = "/var/lib/tapyr/tapyr.db" }, "state.path"},
		{"log", func(s *config.SettingsSpec) { s.Log.Level = "debug" }, "log"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var logs bytes.Buffer
			c := &Collector{settings: base(), logger: slog.New(slog.NewTextHandler(&logs, nil))}
			next := base()
			tt.change(next)
			c.warnRestartSettings(next)
			got := logs.String()
			if tt.want == "" && got != "" {
				t.Errorf("unexpected warning: %s", got)
			}
			if tt.want != "" && (strings.Count(got, "take effect after a restart") != 1 || !strings.Contains(got, "settings="+tt.want)) {
				t.Errorf("want one warning about %s, got: %s", tt.want, got)
			}
		})
	}
}

func TestServerURLs(t *testing.T) {
	off := false
	for _, tc := range []struct {
		name       string
		addr       string
		server     config.ServerSettings
		prometheus bool
		want       string
	}{
		{"local", "127.0.0.1:9790", config.ServerSettings{}, true, "[ui http://127.0.0.1:9790/ui/ metrics http://127.0.0.1:9790/metrics api http://127.0.0.1:9790/api/v1/]"},
		{"wildcard, no prometheus output", "0.0.0.0:9790", config.ServerSettings{}, false, "[ui http://localhost:9790/ui/ api http://localhost:9790/api/v1/]"},
		{"IPv6 wildcard", "[::]:9790", config.ServerSettings{}, false, "[ui http://localhost:9790/ui/ api http://localhost:9790/api/v1/]"},
		{"IPv6", "[::1]:9790", config.ServerSettings{}, false, "[ui http://[::1]:9790/ui/ api http://[::1]:9790/api/v1/]"},
		{"TLS, UI off", "10.0.0.1:443", config.ServerSettings{TLS: &config.ServerTLS{}, UI: config.UISettings{Enabled: &off}}, true, "[metrics https://10.0.0.1:443/metrics api https://10.0.0.1:443/api/v1/]"},
		{"not an address", "nope", config.ServerSettings{}, true, "[]"},
	} {
		if got := fmt.Sprint(serverURLs(tc.addr, tc.server, tc.prometheus)); got != tc.want {
			t.Errorf("%s: serverURLs() = %s, want %s", tc.name, got, tc.want)
		}
	}
}
