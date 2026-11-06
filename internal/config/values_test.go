package config

import (
	"encoding/json"
	"regexp"
	"testing"
	"time"
)

func TestDuration(t *testing.T) {
	tests := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{in: `"30s"`, want: 30 * time.Second},
		{in: `"1h30m"`, want: 90 * time.Minute},
		{in: `"1.5s"`, want: 1500 * time.Millisecond},
		{in: `"250ms"`, want: 250 * time.Millisecond},
		{in: `"0s"`, want: 0},
		{in: `"1d"`, wantErr: true},
		{in: `"soon"`, wantErr: true},
		{in: `30`, wantErr: true},
	}
	for _, tt := range tests {
		var d Duration
		err := json.Unmarshal([]byte(tt.in), &d)
		if (err != nil) != tt.wantErr {
			t.Errorf("Unmarshal(%s) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			continue
		}
		if !tt.wantErr && d.Std() != tt.want {
			t.Errorf("Unmarshal(%s) = %v, want %v", tt.in, d.Std(), tt.want)
		}
		if !tt.wantErr {
			roundTrip(t, d)
		}
	}
}

func TestRate(t *testing.T) {
	tests := []struct {
		in        string
		count     float64
		per       time.Duration
		perSecond float64
		wantErr   bool
	}{
		{in: `"10/s"`, count: 10, per: time.Second, perSecond: 10},
		{in: `"600/m"`, count: 600, per: time.Minute, perSecond: 10},
		{in: `"1.5/s"`, count: 1.5, per: time.Second, perSecond: 1.5},
		{in: `"3600/h"`, count: 3600, per: time.Hour, perSecond: 1},
		{in: `"0/s"`, count: 0, per: time.Second, perSecond: 0}, // rejected by semantic validation
		{in: `"10/x"`, wantErr: true},
		{in: `"10"`, wantErr: true},
		{in: `"-1/s"`, wantErr: true},
		{in: `10`, wantErr: true},
	}
	for _, tt := range tests {
		var r Rate
		err := json.Unmarshal([]byte(tt.in), &r)
		if (err != nil) != tt.wantErr {
			t.Errorf("Unmarshal(%s) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			continue
		}
		if tt.wantErr {
			continue
		}
		if r.Count != tt.count || r.Per != tt.per || r.PerSecond() != tt.perSecond {
			t.Errorf("Unmarshal(%s) = %+v (%v/s)", tt.in, r, r.PerSecond())
		}
		roundTrip(t, r)
	}
}

func TestPercent(t *testing.T) {
	tests := []struct {
		in      string
		want    float64
		wantErr bool
	}{
		{in: `"10%"`, want: 0.1},
		{in: `"2.5%"`, want: 0.025},
		{in: `"0%"`, want: 0},
		{in: `"150%"`, want: 1.5}, // range is checked per field by semantic validation
		{in: `"10"`, wantErr: true},
		{in: `"abc%"`, wantErr: true},
		{in: `0.1`, wantErr: true},
	}
	for _, tt := range tests {
		var p Percent
		err := json.Unmarshal([]byte(tt.in), &p)
		if (err != nil) != tt.wantErr {
			t.Errorf("Unmarshal(%s) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			continue
		}
		if !tt.wantErr {
			if p.Fraction() != tt.want {
				t.Errorf("Unmarshal(%s) = %v, want %v", tt.in, p.Fraction(), tt.want)
			}
			roundTrip(t, p)
		}
	}
}

func TestIntOrAuto(t *testing.T) {
	tests := []struct {
		in       string
		wantAuto bool
		wantN    int64
		wantErr  bool
	}{
		{in: `5000`, wantN: 5000},
		{in: `"auto"`, wantAuto: true},
		{in: `"5000"`, wantErr: true},
		{in: `1.5`, wantErr: true},
		{in: `true`, wantErr: true},
	}
	for _, tt := range tests {
		var v IntOrAuto
		err := json.Unmarshal([]byte(tt.in), &v)
		if (err != nil) != tt.wantErr {
			t.Errorf("Unmarshal(%s) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			continue
		}
		if !tt.wantErr {
			if v.Auto != tt.wantAuto || v.Value != tt.wantN {
				t.Errorf("Unmarshal(%s) = %+v", tt.in, v)
			}
			roundTrip(t, v)
		}
	}
}

func TestValue(t *testing.T) {
	tests := []struct {
		in      string
		want    Value
		wantErr bool
	}{
		{in: `"application/json"`, want: Value{Literal: "application/json"}},
		{in: `""`, want: Value{}},
		{in: `{"secret":"apiKey"}`, want: Value{Secret: "apiKey"}},
		{in: `{"expr":"vars.x"}`, want: Value{Expr: "vars.x"}},
		{in: `{"secret":"a","expr":"b"}`, wantErr: true},
		{in: `{}`, wantErr: true},
		{in: `{"env":"X"}`, wantErr: true},
		{in: `42`, wantErr: true},
	}
	for _, tt := range tests {
		var v Value
		err := json.Unmarshal([]byte(tt.in), &v)
		if (err != nil) != tt.wantErr {
			t.Errorf("Unmarshal(%s) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			continue
		}
		if !tt.wantErr {
			if v != tt.want {
				t.Errorf("Unmarshal(%s) = %+v, want %+v", tt.in, v, tt.want)
			}
			roundTrip(t, v)
		}
	}
}

func TestSecretRef(t *testing.T) {
	tests := []struct {
		in      string
		want    SecretRef
		wantErr bool
	}{
		{in: `{"env":"TAPYR_KEY"}`, want: SecretRef{Env: "TAPYR_KEY"}},
		{in: `{"file":"/run/secrets/key"}`, want: SecretRef{File: "/run/secrets/key"}},
		{in: `{"env":"A","file":"/b"}`, wantErr: true},
		{in: `{}`, wantErr: true},
		{in: `"inline-secret"`, wantErr: true},
	}
	for _, tt := range tests {
		var s SecretRef
		err := json.Unmarshal([]byte(tt.in), &s)
		if (err != nil) != tt.wantErr {
			t.Errorf("Unmarshal(%s) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			continue
		}
		if !tt.wantErr && s != tt.want {
			t.Errorf("Unmarshal(%s) = %+v, want %+v", tt.in, s, tt.want)
		}
	}
}

func TestStringOrSecret(t *testing.T) {
	tests := []struct {
		in      string
		want    StringOrSecret
		wantErr bool
	}{
		{in: `"tenant-a"`, want: StringOrSecret{Literal: "tenant-a"}},
		{in: `{"env":"TOKEN"}`, want: StringOrSecret{Secret: &SecretRef{Env: "TOKEN"}}},
		{in: `{"secret":"x"}`, wantErr: true},
		{in: `1`, wantErr: true},
	}
	for _, tt := range tests {
		var s StringOrSecret
		err := json.Unmarshal([]byte(tt.in), &s)
		if (err != nil) != tt.wantErr {
			t.Errorf("Unmarshal(%s) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			continue
		}
		if tt.wantErr {
			continue
		}
		if s.Literal != tt.want.Literal || (s.Secret == nil) != (tt.want.Secret == nil) ||
			(s.Secret != nil && *s.Secret != *tt.want.Secret) {
			t.Errorf("Unmarshal(%s) = %+v, want %+v", tt.in, s, tt.want)
		}
		roundTrip(t, s)
	}
}

// TestSchemaPatternsMatchParsers keeps the schema patterns and the Go parsers
// in agreement: every string accepted by a pattern must parse.
func TestSchemaPatternsMatchParsers(t *testing.T) {
	tests := []struct {
		pattern string
		parse   func(string) error
		samples []string
	}{
		{
			durationPattern, func(s string) error { _, err := time.ParseDuration(s); return err },
			[]string{"30s", "1h30m", "1.5s", "250ms", "10us", "0s", "1h0m0s"},
		},
		{
			ratePattern, func(s string) error { _, err := parseRate(s); return err },
			[]string{"10/s", "600/m", "1.5/h", "0/s"},
		},
		{
			percentPattern, func(s string) error { _, err := parsePercent(s); return err },
			[]string{"10%", "2.5%", "0%", "100%"},
		},
		{
			datePattern, func(s string) error { _, err := ParseDate(s); return err },
			[]string{"2026-11-04", "2026-11", "2028-02-29", "1999-12-31"},
		},
	}
	for _, tt := range tests {
		re := regexp.MustCompile(tt.pattern)
		for _, s := range tt.samples {
			if !re.MatchString(s) {
				t.Errorf("pattern %q does not match %q", tt.pattern, s)
			}
			if err := tt.parse(s); err != nil {
				t.Errorf("parse(%q) = %v, but the pattern accepts it", s, err)
			}
		}
	}
}

func roundTrip[T any](t *testing.T, v T) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("Marshal(%+v) = %v", v, err)
	}
	var got T
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal(Marshal(%+v)) = %v (json %s)", v, err, b)
	}
	b2, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != string(b2) {
		t.Fatalf("round trip changed value: %s -> %s", b, b2)
	}
}

func TestRenderTemplate(t *testing.T) {
	tests := []struct {
		text   string
		params map[string]string
		want   string
	}{
		{`{{ .params.baseURL | default "https://api.example.invalid/v1" }}`, nil, "https://api.example.invalid/v1"},
		{`{{ .params.baseURL | default "https://x" | trimSuffix "/" }}`, map[string]string{"baseURL": "https://eu.example.invalid/"}, "https://eu.example.invalid"},
		{`{{ .params.region | upper }}-{{ .params.region | lower }}`, map[string]string{"region": "Eu"}, "EU-eu"},
		{`static`, nil, "static"},
	}
	for _, tt := range tests {
		got, err := RenderTemplate("t", tt.text, tt.params)
		if err != nil || got != tt.want {
			t.Errorf("RenderTemplate(%q) = %q, %v; want %q", tt.text, got, err, tt.want)
		}
	}
	if _, err := RenderTemplate("t", "{{ .params.x | nope }}", nil); err == nil {
		t.Error("unknown function accepted")
	}
	if got := PathVars("/orgs/{orgId}/devices/{serial}"); len(got) != 2 || got[0] != "orgId" || got[1] != "serial" {
		t.Errorf("PathVars() = %v", got)
	}
}

func TestParseDate(t *testing.T) {
	tests := []struct {
		in   string
		want time.Time
		ok   bool
	}{
		{"2026-11-04", time.Date(2026, time.November, 4, 0, 0, 0, 0, time.UTC), true},
		{"2026-11", time.Date(2026, time.November, 1, 0, 0, 0, 0, time.UTC), true},
		{"2026-02-30", time.Time{}, false},
		{"2026-13", time.Time{}, false},
		{"November 2026", time.Time{}, false},
		{"", time.Time{}, false},
	}
	for _, tt := range tests {
		got, err := ParseDate(tt.in)
		if (err == nil) != tt.ok || !got.Equal(tt.want) {
			t.Errorf("ParseDate(%q) = %v, %v; want %v, ok %v", tt.in, got, err, tt.want, tt.ok)
		}
	}
}
