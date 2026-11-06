package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/invopop/jsonschema"
)

// Patterns shared by the JSON Schemas and the Go parsers. The schema rejects
// malformed values with a position; the parsers then never fail on values
// the schema accepted (see TestSchemaPatternsMatchParsers).
const (
	durationPattern   = `^([0-9]+(\.[0-9]+)?(ns|us|µs|ms|s|m|h))+$`
	ratePattern       = `^[0-9]+(\.[0-9]+)?/(s|m|h)$`
	percentPattern    = `^[0-9]+(\.[0-9]+)?%$`
	namePattern       = `^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`
	identifierPattern = `^[A-Za-z_][A-Za-z0-9_]*$`
	labelNamePattern  = `^[a-zA-Z_][a-zA-Z0-9_]*$`
	metricNamePattern = `^[a-zA-Z_:][a-zA-Z0-9_:]*$`
	infoNamePattern   = `^[a-zA-Z_:][a-zA-Z0-9_:]*_info$`
	resetAtPattern    = `^([01][0-9]|2[0-3]):[0-5][0-9]$`
	semverPattern     = `^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$`
	datePattern       = `^[0-9]{4}-(0[1-9]|1[0-2])(-(0[1-9]|[12][0-9]|3[01]))?$`
	handlePattern     = `^[A-Za-z0-9]([A-Za-z0-9-]{0,37}[A-Za-z0-9])?$`
)

// patternHints turns schema patterns into human-readable error messages.
var patternHints = map[string]string{
	durationPattern:   "must be a duration such as 30s, 5m or 1h30m",
	ratePattern:       "must be a rate such as 10/s, 600/m or 100/h",
	percentPattern:    `must be a percentage such as "10%"`,
	namePattern:       "must be a DNS label: lowercase letters, digits and hyphens, starting and ending with a letter or digit, at most 63 characters",
	identifierPattern: "must be an identifier: letters, digits and underscores, not starting with a digit",
	labelNamePattern:  "must be a valid label name: letters, digits and underscores, not starting with a digit",
	metricNamePattern: "must be a valid metric name: letters, digits, underscores and colons, not starting with a digit",
	infoNamePattern:   "must be a valid metric name ending with _info",
	resetAtPattern:    "must be a time of day in 24h HH:MM format",
	semverPattern:     "must be a semantic version such as 0.1.0",
	datePattern:       "must be a date such as 2026-11-04 or 2026-11",
	handlePattern:     "must be a GitHub handle, without @",
	`^/`:              "must start with /",
	`^auto$`:          `must be a positive integer or "auto"`,
}

// Duration is a Go duration string such as "30s", "5m" or "1h30m". Days are
// not supported: use hours ("24h").
type Duration time.Duration

// Std returns the duration as a time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// MarshalJSON implements json.Marshaler.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// UnmarshalJSON implements json.Unmarshaler.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return errors.New("duration must be a string such as 30s")
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

// JSONSchema implements the schema reflector hook.
func (Duration) JSONSchema() *jsonschema.Schema {
	return &jsonschema.Schema{Type: "string", Pattern: durationPattern, Examples: []any{"30s", "5m", "1h30m"}}
}

// Rate is a number of events per time unit, written "<n>/<unit>" with unit
// s, m or h ("10/s", "600/m").
type Rate struct {
	Count float64
	Per   time.Duration
}

// PerSecond returns the rate in events per second.
func (r Rate) PerSecond() float64 {
	if r.Per == 0 {
		return 0
	}
	return r.Count / r.Per.Seconds()
}

// String returns the "<n>/<unit>" form.
func (r Rate) String() string {
	unit := map[time.Duration]string{time.Second: "s", time.Minute: "m", time.Hour: "h"}[r.Per]
	return strconv.FormatFloat(r.Count, 'f', -1, 64) + "/" + unit
}

// MarshalJSON implements json.Marshaler.
func (r Rate) MarshalJSON() ([]byte, error) { return json.Marshal(r.String()) }

// UnmarshalJSON implements json.Unmarshaler.
func (r *Rate) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return errors.New("rate must be a string such as 10/s")
	}
	v, err := parseRate(s)
	if err != nil {
		return err
	}
	*r = v
	return nil
}

var (
	rateRe       = regexp.MustCompile(ratePattern)
	identifierRe = regexp.MustCompile(identifierPattern)
)

func parseRate(s string) (Rate, error) {
	if !rateRe.MatchString(s) {
		return Rate{}, fmt.Errorf("invalid rate %q (want <n>/s, <n>/m or <n>/h)", s)
	}
	n, unit, _ := strings.Cut(s, "/")
	count, err := strconv.ParseFloat(n, 64)
	if err != nil {
		return Rate{}, fmt.Errorf("invalid rate %q: %w", s, err)
	}
	per := map[string]time.Duration{"s": time.Second, "m": time.Minute, "h": time.Hour}[unit]
	return Rate{Count: count, Per: per}, nil
}

// JSONSchema implements the schema reflector hook.
func (Rate) JSONSchema() *jsonschema.Schema {
	return &jsonschema.Schema{Type: "string", Pattern: ratePattern, Examples: []any{"10/s", "600/m"}}
}

// Percent is a percentage written as a string ("10%", "2.5%"). It is stored
// as a fraction (0.1 for "10%").
type Percent float64

// Fraction returns the percentage as a fraction of 1.
func (p Percent) Fraction() float64 { return float64(p) }

// MarshalJSON implements json.Marshaler.
func (p Percent) MarshalJSON() ([]byte, error) {
	return json.Marshal(strconv.FormatFloat(float64(p)*100, 'f', -1, 64) + "%")
}

// UnmarshalJSON implements json.Unmarshaler.
func (p *Percent) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return errors.New(`percentage must be a string such as "10%"`)
	}
	v, err := parsePercent(s)
	if err != nil {
		return err
	}
	*p = v
	return nil
}

var percentRe = regexp.MustCompile(percentPattern)

func parsePercent(s string) (Percent, error) {
	if !percentRe.MatchString(s) {
		return 0, fmt.Errorf(`invalid percentage %q (want a string such as "10%%")`, s)
	}
	v, err := strconv.ParseFloat(strings.TrimSuffix(s, "%"), 64)
	if err != nil {
		return 0, fmt.Errorf("invalid percentage %q: %w", s, err)
	}
	return Percent(v / 100), nil
}

// JSONSchema implements the schema reflector hook.
func (Percent) JSONSchema() *jsonschema.Schema {
	return &jsonschema.Schema{Type: "string", Pattern: percentPattern, Examples: []any{"10%"}}
}

// IntOrAuto is a positive integer, or "auto" when the value is learned at
// runtime (quota maximum learned from response headers).
type IntOrAuto struct {
	Auto  bool
	Value int64
}

// MarshalJSON implements json.Marshaler.
func (v IntOrAuto) MarshalJSON() ([]byte, error) {
	if v.Auto {
		return json.Marshal("auto")
	}
	return json.Marshal(v.Value)
}

// UnmarshalJSON implements json.Unmarshaler.
func (v *IntOrAuto) UnmarshalJSON(b []byte) error {
	if string(b) == `"auto"` {
		*v = IntOrAuto{Auto: true}
		return nil
	}
	var n int64
	if err := json.Unmarshal(b, &n); err != nil {
		return errors.New(`must be an integer or "auto"`)
	}
	*v = IntOrAuto{Value: n}
	return nil
}

// JSONSchema implements the schema reflector hook. Type-specific keywords
// (minimum for integers, pattern for strings) keep error messages precise
// without oneOf.
func (IntOrAuto) JSONSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type:    "",
		Extras:  map[string]any{"type": []string{"integer", "string"}},
		Minimum: json.Number("1"),
		Pattern: `^auto$`,
	}
}

// Value is a literal string, a reference to a secret declared by the
// provider ({secret: name}) or a CEL expression ({expr: "..."}).
type Value struct {
	Literal string
	Secret  string
	Expr    string
}

// MarshalJSON implements json.Marshaler.
func (v Value) MarshalJSON() ([]byte, error) {
	switch {
	case v.Secret != "":
		return json.Marshal(map[string]string{"secret": v.Secret})
	case v.Expr != "":
		return json.Marshal(map[string]string{"expr": v.Expr})
	}
	return json.Marshal(v.Literal)
}

// UnmarshalJSON implements json.Unmarshaler.
func (v *Value) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) > 0 && b[0] == '"' {
		*v = Value{}
		return json.Unmarshal(b, &v.Literal)
	}
	var obj struct {
		Secret string `json:"secret"`
		Expr   string `json:"expr"`
	}
	if err := strictUnmarshal(b, &obj); err != nil {
		return errors.New("value must be a string, {secret: name} or {expr: cel}")
	}
	if (obj.Secret == "") == (obj.Expr == "") {
		return errors.New("value must set exactly one of secret or expr")
	}
	*v = Value{Secret: obj.Secret, Expr: obj.Expr}
	return nil
}

// JSONSchema implements the schema reflector hook.
func (Value) JSONSchema() *jsonschema.Schema {
	props := jsonschema.NewProperties()
	props.Set("secret", &jsonschema.Schema{
		Type: "string", Pattern: identifierPattern,
		Description: "Name of a secret declared by the provider and supplied by the account.",
	})
	props.Set("expr", &jsonschema.Schema{
		Type: "string", MinLength: ptr(uint64(1)),
		Description: "CEL expression evaluated at runtime.",
	})
	return &jsonschema.Schema{
		Extras:               map[string]any{"type": []string{"string", "object"}},
		Description:          "A literal string, {secret: name} or {expr: cel}.",
		Properties:           props,
		AdditionalProperties: jsonschema.FalseSchema,
		MinProperties:        ptr(uint64(1)),
		MaxProperties:        ptr(uint64(1)),
	}
}

// SecretRef points to a secret outside the configuration: exactly one of an
// environment variable or a file. Secrets are never written inline.
type SecretRef struct {
	// Env is the name of an environment variable holding the secret.
	Env string `json:"env,omitempty"`
	// File is the path of a file holding the secret, relative to the
	// declaring file. Re-read on reload.
	File string `json:"file,omitempty"`
}

// UnmarshalJSON implements json.Unmarshaler.
func (s *SecretRef) UnmarshalJSON(b []byte) error {
	type plain SecretRef
	var v plain
	if err := strictUnmarshal(b, &v); err != nil {
		return errors.New("secret must be {env: NAME} or {file: /path}")
	}
	if (v.Env == "") == (v.File == "") {
		return errors.New("secret must set exactly one of env or file")
	}
	*s = SecretRef(v)
	return nil
}

// JSONSchema implements the schema reflector hook.
func (SecretRef) JSONSchema() *jsonschema.Schema {
	return withSecretRefProperties(&jsonschema.Schema{
		Type:        "object",
		Description: "A secret read from an environment variable ({env: NAME}) or a file ({file: /path}).",
	})
}

// withSecretRefProperties adds the {env} / {file} object keywords to s.
func withSecretRefProperties(s *jsonschema.Schema) *jsonschema.Schema {
	s.Properties = jsonschema.NewProperties()
	s.Properties.Set("env", &jsonschema.Schema{
		Type: "string", MinLength: ptr(uint64(1)),
		Description: "Name of an environment variable holding the secret.",
	})
	s.Properties.Set("file", &jsonschema.Schema{
		Type: "string", MinLength: ptr(uint64(1)),
		Description: "Path of a file holding the secret, relative to the declaring file.",
	})
	s.AdditionalProperties = jsonschema.FalseSchema
	s.MinProperties = ptr(uint64(1))
	s.MaxProperties = ptr(uint64(1))
	return s
}

// StringOrSecret is a literal string or a SecretRef. It is used for output
// header values.
type StringOrSecret struct {
	Literal string
	Secret  *SecretRef
}

// MarshalJSON implements json.Marshaler.
func (s StringOrSecret) MarshalJSON() ([]byte, error) {
	if s.Secret != nil {
		return json.Marshal(s.Secret)
	}
	return json.Marshal(s.Literal)
}

// UnmarshalJSON implements json.Unmarshaler.
func (s *StringOrSecret) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) > 0 && b[0] == '"' {
		*s = StringOrSecret{}
		return json.Unmarshal(b, &s.Literal)
	}
	var ref SecretRef
	if err := json.Unmarshal(b, &ref); err != nil {
		return err
	}
	*s = StringOrSecret{Secret: &ref}
	return nil
}

// JSONSchema implements the schema reflector hook.
func (StringOrSecret) JSONSchema() *jsonschema.Schema {
	return withSecretRefProperties(&jsonschema.Schema{
		Extras:      map[string]any{"type": []string{"string", "object"}},
		Description: "A literal string, {env: NAME} or {file: /path}.",
	})
}

// strictUnmarshal decodes JSON rejecting unknown fields.
func strictUnmarshal(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func ptr[T any](v T) *T { return &v }

// ParseDate parses a date of the catalog metadata: YYYY-MM-DD, or YYYY-MM
// when the day is not known.
func ParseDate(s string) (time.Time, error) {
	if t, err := time.Parse(time.DateOnly, s); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01", s); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("%q is not a date (YYYY-MM-DD or YYYY-MM)", s)
}
