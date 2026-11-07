package collector

import (
	"bytes"
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
