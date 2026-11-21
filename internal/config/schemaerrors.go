package config

import (
	"fmt"
	"math/big"
	"slices"
	"strconv"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

var printer = message.NewPrinter(language.English)

// exactlyOneOf names the alternatives of the one-field objects.
//
//nolint:gosec // G101: field names, not credentials.
var exactlyOneOf = map[string]string{
	"SecretRef":          "env or file",
	"SimulationResponse": "file or template",
	"StringOrSecret":     "env or file",
	"Value":              "secret or expr",
}

// schemaDiagnostics flattens a validation error tree into one diagnostic
// per failing leaf, with positions and human-readable messages.
// schemaAt returns the schema at a schema URL, for the fields an object
// allows (the "did you mean" of unknown fields) and the pattern a string
// must match (the hint of a value of the wrong type); it may be nil.
func schemaDiagnostics(ve *jsonschema.ValidationError, idx *positionIndex, schemaAt func(schemaURL string) map[string]any) Diagnostics {
	var out Diagnostics
	var walk func(e *jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if pn, ok := e.ErrorKind.(*kind.PropertyNames); ok {
			for _, tokens := range propertyNameLocations(e, pn.Property, idx) {
				for _, c := range e.Causes {
					out = append(out, propertyNameDiagnostic(c, idx, tokens))
				}
			}
			return
		}
		if len(e.Causes) == 0 {
			out = append(out, leafDiagnostics(e, idx, schemaAt)...)
			return
		}
		for _, c := range e.Causes {
			walk(c)
		}
	}
	walk(ve)
	return out
}

func leafDiagnostics(e *jsonschema.ValidationError, idx *positionIndex, schemaAt func(string) map[string]any) Diagnostics {
	loc := e.InstanceLocation
	var schema map[string]any
	if schemaAt != nil {
		schema = schemaAt(e.SchemaURL)
	}
	diag := func(tokens []string, preferKey bool, msg string) Diagnostic {
		return Diagnostic{Pos: idx.position(tokens, preferKey), Path: idx.path(tokens), Message: msg}
	}
	switch k := e.ErrorKind.(type) {
	case *kind.AdditionalProperties:
		out := make(Diagnostics, 0, len(k.Properties))
		props, _ := schema["properties"].(map[string]any)
		allowed := make([]string, 0, len(props))
		for name := range props {
			allowed = append(allowed, name)
		}
		slices.Sort(allowed)
		for _, p := range k.Properties {
			out = append(out, diag(child(loc, p), true, fmt.Sprintf("unknown field %q%s", p, didYouMean(p, allowed))))
		}
		return out
	case *kind.Required:
		out := make(Diagnostics, 0, len(k.Missing))
		for _, m := range k.Missing {
			out = append(out, diag(loc, false, fmt.Sprintf("missing required field %q", m)))
		}
		return out
	case *kind.Type:
		if strings.HasSuffix(e.SchemaURL, "/$defs/SecretRef") {
			return Diagnostics{diag(loc, false, "secrets are never written inline: use {env: NAME} or {file: /path}")}
		}
		if k.Got == "null" && slices.Contains(k.Want, "object") {
			// "secrets:" with nothing after it: its fields were not
			// indented under it, and appear next to it.
			return Diagnostics{diag(loc, false, "is empty: its fields must be indented under it (more than this key)")}
		}
		msg := fmt.Sprintf("invalid type: got %s, want %s", k.Got, strings.Join(k.Want, " or "))
		if slices.Contains(k.Want, "string") && (k.Got == "number" || k.Got == "integer" || k.Got == "boolean") {
			// A duration, a size or a rate written as a bare number
			// ("interval: 5"): quoting it would only lead to the next
			// error, since it lacks a unit. Say what the value looks like.
			if pattern, _ := schema["pattern"].(string); patternHints[pattern] != "" {
				if raw, ok := idx.raw(loc); ok {
					return Diagnostics{diag(loc, false, fmt.Sprintf("%s %s", raw, patternHints[pattern]))}
				}
			}
			if raw, ok := idx.raw(loc); ok {
				msg += fmt.Sprintf(" (quote the value: %q)", raw)
			}
		}
		return Diagnostics{diag(loc, false, msg)}
	case *kind.Pattern:
		if hint, ok := patternHints[k.Want]; ok {
			return Diagnostics{diag(loc, false, fmt.Sprintf("%q %s", k.Got, hint))}
		}
		return Diagnostics{diag(loc, false, fmt.Sprintf("%q does not match pattern %s", k.Got, k.Want))}
	case *kind.Enum:
		want := make([]string, len(k.Want))
		for i, w := range k.Want {
			want[i] = quoteAny(w)
		}
		return Diagnostics{diag(loc, false, fmt.Sprintf("invalid value %s (want one of %s)", quoteAny(k.Got), strings.Join(want, ", ")))}
	case *kind.Const:
		return Diagnostics{diag(loc, false, fmt.Sprintf("invalid value %s (want %s)", quoteAny(k.Got), quoteAny(k.Want)))}
	case *kind.MinProperties, *kind.MaxProperties:
		for def, fields := range exactlyOneOf {
			if strings.HasSuffix(e.SchemaURL, "/$defs/"+def) {
				return Diagnostics{diag(loc, false, "must set exactly one of "+fields)}
			}
		}
		return Diagnostics{diag(loc, false, "must set exactly one field")}
	case *kind.Minimum:
		return Diagnostics{diag(loc, false, fmt.Sprintf("must be at least %s (got %s)", ratString(k.Want), ratString(k.Got)))}
	case *kind.Maximum:
		return Diagnostics{diag(loc, false, fmt.Sprintf("must be at most %s (got %s)", ratString(k.Want), ratString(k.Got)))}
	case *kind.ExclusiveMinimum:
		return Diagnostics{diag(loc, false, fmt.Sprintf("must be greater than %s (got %s)", ratString(k.Want), ratString(k.Got)))}
	case *kind.MinItems:
		noun := "items"
		if k.Want == 1 {
			noun = "item"
		}
		return Diagnostics{diag(loc, false, fmt.Sprintf("must have at least %d %s (got %d)", k.Want, noun, k.Got))}
	case *kind.Format:
		return Diagnostics{diag(loc, false, fmt.Sprintf("%s is not a valid %s", quoteAny(k.Got), k.Want))}
	}
	return Diagnostics{diag(loc, false, e.ErrorKind.LocalizedString(printer))}
}

// propertyNameLocations works around jsonschema/v6 reusing its location
// buffer for propertyNames errors (InstanceLocation is overwritten after
// validation). The map field is recovered from the schema location
// (.../properties/<field>/propertyNames) and every key named property under
// such a field is reported: all maps sharing a field name share the key rule.
func propertyNameLocations(e *jsonschema.ValidationError, property string, idx *positionIndex) [][]string {
	_, frag, _ := strings.Cut(e.SchemaURL, "#")
	segs := strings.Split(frag, "/")
	field := ""
	if n := len(segs); n >= 3 && segs[n-1] == "propertyNames" && segs[n-3] == "properties" {
		field = segs[n-2]
	}
	var out [][]string
	for ptr := range idx.nodes {
		tokens := parsePointer(ptr)
		n := len(tokens)
		if n >= 2 && tokens[n-1] == property && (field == "" || tokens[n-2] == field) {
			out = append(out, tokens)
		}
	}
	slices.SortFunc(out, func(a, b []string) int { return strings.Compare(pointer(a), pointer(b)) })
	return out
}

func ratString(r *big.Rat) string {
	if r == nil {
		return "?"
	}
	if r.IsInt() {
		return r.Num().String()
	}
	f, _ := r.Float64()
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// propertyNameDiagnostic reports an invalid map key at the key position.
func propertyNameDiagnostic(e *jsonschema.ValidationError, idx *positionIndex, tokens []string) Diagnostic {
	msg := e.ErrorKind.LocalizedString(printer)
	if p, ok := e.ErrorKind.(*kind.Pattern); ok {
		if hint, ok := patternHints[p.Want]; ok {
			msg = hint
		}
	}
	key := tokens[len(tokens)-1]
	return Diagnostic{
		Pos:     idx.position(tokens, true),
		Path:    idx.path(tokens),
		Message: fmt.Sprintf("invalid key %q: %s", key, msg),
	}
}
