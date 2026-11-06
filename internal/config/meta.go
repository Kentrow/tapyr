// Package config defines the Tapyr configuration model (tapyr/v1alpha1),
// loads YAML documents from files and directories, validates them against
// the generated JSON Schemas and applies semantic validation and defaults.
//
// The Go types in this package are the single source of truth: JSON Schemas
// are generated from them (make schema) and their doc comments become the
// schema descriptions. See docs/spec/03-configuration.md.
package config

import (
	"slices"

	"github.com/invopop/jsonschema"
)

// APIVersion is the configuration API version supported by this release.
const APIVersion = "tapyr/v1alpha1"

// Kind identifies the type of a configuration document.
type Kind string

// Document kinds accepted in user configuration.
const (
	KindProvider    Kind = "Provider"
	KindJobTemplate Kind = "JobTemplate"
	KindAccount     Kind = "Account"
	KindJob         Kind = "Job"
	KindOutput      Kind = "Output"
	KindSettings    Kind = "Settings"
	// KindSimulation is only valid in provider packages (sim/sim.yaml).
	KindSimulation Kind = "Simulation"
	// KindScenario is only valid in provider packages (sim/scenarios/).
	KindScenario Kind = "Scenario"
)

// kinds are the document kinds accepted in user configuration, in
// documentation order; allKinds adds the kinds of provider packages.
var (
	kinds    = []Kind{KindProvider, KindJobTemplate, KindAccount, KindJob, KindOutput, KindSettings}
	allKinds = append(slices.Clone(kinds), KindSimulation, KindScenario)
)

// Kinds returns the document kinds accepted in user configuration, in
// documentation order.
func Kinds() []Kind { return slices.Clone(kinds) }

// AllKinds returns every kind with a schema: the user kinds and the kinds
// of provider packages.
func AllKinds() []Kind { return slices.Clone(allKinds) }

// packageKinds are valid only inside provider packages (sim/ directory).
var packageKinds = map[Kind]bool{KindSimulation: true, KindScenario: true}

// Name is a DNS-label-like name: lowercase letters, digits and hyphens, at
// most 63 characters.
type Name string

// JSONSchema implements the schema reflector hook.
func (Name) JSONSchema() *jsonschema.Schema {
	return &jsonschema.Schema{Type: "string", Pattern: namePattern}
}

// Identifier names a param, a secret or a variable. It is usable in CEL
// (vars.organizationId) and in path templates ({organizationId}).
type Identifier string

// JSONSchema implements the schema reflector hook.
func (Identifier) JSONSchema() *jsonschema.Schema {
	return &jsonschema.Schema{Type: "string", Pattern: identifierPattern}
}

// Expr is a CEL expression, compiled and type-checked by tapyr validate.
type Expr string

// JSONSchema implements the schema reflector hook.
func (Expr) JSONSchema() *jsonschema.Schema {
	return &jsonschema.Schema{Type: "string", MinLength: ptr(uint64(1)), Description: "CEL expression."}
}

// Template is a Go text/template string evaluated once per account and
// target, with .params holding the account params overridden by the target
// params. Allowed functions: default, trimSuffix, lower, upper.
type Template string

// JSONSchema implements the schema reflector hook.
func (Template) JSONSchema() *jsonschema.Schema {
	return &jsonschema.Schema{Type: "string", Description: "Go text/template evaluated per account and target."}
}

// ObjectMeta identifies a document.
type ObjectMeta struct {
	// Name of the document, unique per kind.
	Name Name `json:"name" jsonschema:"required"`
	// Description is free text for humans.
	Description string `json:"description,omitempty"`
}

// ProviderMeta identifies a provider package.
type ProviderMeta struct {
	// Name of the provider, unique across built-in and user packages.
	Name Name `json:"name" jsonschema:"required"`
	// Version of the provider package (semver). Breaking changes to metric
	// names or labels bump the major version.
	Version string `json:"version" jsonschema:"required"`
	// Description is free text for humans.
	Description string `json:"description,omitempty"`
	// Docs links to the vendor API documentation.
	Docs string `json:"docs,omitempty" jsonschema:"format=uri"`
	// DisplayName is the name of the provider in the catalog, for example
	// "Cisco Meraki Dashboard". Like categories, maintainers, tier and
	// validation, it is optional, and required by tapyr provider lint
	// --catalog (ADR 0011).
	DisplayName string `json:"displayName,omitempty"`
	// Categories of the provider, from providers/categories.yaml, the main
	// one first.
	Categories []Name `json:"categories,omitempty" jsonschema:"uniqueItems=true"`
	// Maintainers are the GitHub handles, without @, of the people who answer
	// for the package.
	Maintainers []string `json:"maintainers,omitempty" jsonschema:"uniqueItems=true"`
	// Tier says who maintains the package: official (the Tapyr maintainers)
	// or community (the maintainers listed).
	Tier string `json:"tier,omitempty" jsonschema:"enum=official,enum=community"`
	// Validation says how the package was last tested.
	Validation *Validation `json:"validation,omitempty"`
}

// JSONSchemaExtend implements the schema reflector hook.
func (ProviderMeta) JSONSchemaExtend(s *jsonschema.Schema) {
	setPattern(s, "version", semverPattern)
	if p, ok := s.Properties.Get("maintainers"); ok && p.Items != nil {
		p.Items.Pattern = handlePattern
	}
}

// Validation records the last validation of a provider package.
type Validation struct {
	// Status is simulator (tested against its simulator only) or
	// real-platform (run against the real API).
	Status string `json:"status" jsonschema:"required,enum=simulator,enum=real-platform"`
	// Date of the validation, YYYY-MM-DD or YYYY-MM. Required by
	// tapyr provider lint --catalog with status real-platform.
	Date string `json:"date,omitempty"`
	// PlatformVersion is the version of the API, platform or firmware
	// validated against, for example "RutOS 07.24.5". Required by
	// tapyr provider lint --catalog with status real-platform.
	PlatformVersion string `json:"platformVersion,omitempty"`
	// Notes is free text: what was covered, what the package was built from.
	Notes string `json:"notes,omitempty"`
}

// JSONSchemaExtend implements the schema reflector hook.
func (Validation) JSONSchemaExtend(s *jsonschema.Schema) {
	setPattern(s, "date", datePattern)
}

// Source records where a document was loaded from, so that semantic errors
// point to file, line and column.
type Source struct {
	// File is the path of the file, as found while loading.
	File string
	// Index is the position of the document in the file (0-based).
	Index int

	positions *positionIndex
}

// Position returns the source position of the value at the given path
// within the document (for example "spec", "limits", "0", "rate").
func (s *Source) Position(tokens ...string) Position {
	if s == nil {
		return Position{}
	}
	return s.positions.position(tokens, false)
}

// Diagnostic returns a diagnostic located at the given path within the
// document.
func (s *Source) Diagnostic(message string, tokens ...string) Diagnostic {
	if s == nil {
		return Diagnostic{Message: message}
	}
	return Diagnostic{Pos: s.positions.position(tokens, false), Path: s.positions.path(tokens), Message: message}
}

// envelope is implemented by every document type.
type envelope interface {
	kind() Kind
	name() string
	source() *Source
}

// Shared schema extension for document types: fixed apiVersion and kind.
func extendEnvelope(s *jsonschema.Schema, kind Kind) {
	if p, ok := s.Properties.Get("apiVersion"); ok {
		p.Const = APIVersion
		p.Type = ""
	}
	if p, ok := s.Properties.Get("kind"); ok {
		p.Const = string(kind)
		p.Type = ""
	}
}

func setPattern(s *jsonschema.Schema, prop, pattern string) {
	if p, ok := s.Properties.Get(prop); ok {
		p.Pattern = pattern
	}
}

// requireWhen adds "if discriminator == value then required: fields" to s.
func requireWhen(s *jsonschema.Schema, discriminator, value string, fields ...string) {
	ifProps := jsonschema.NewProperties()
	ifProps.Set(discriminator, &jsonschema.Schema{Const: value})
	s.AllOf = append(s.AllOf, &jsonschema.Schema{
		If:   &jsonschema.Schema{Properties: ifProps, Required: []string{discriminator}},
		Then: &jsonschema.Schema{Required: fields},
	})
}
