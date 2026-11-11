package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/parser"
	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/kentrow/tapyr/schemas"
)

// Config is a set of configuration documents. After Load or Assemble it is
// validated, with defaults applied.
type Config struct {
	Providers    []*Provider
	JobTemplates []*JobTemplate
	Accounts     []*Account
	Jobs         []*Job
	Outputs      []*Output
	// Settings is never nil after a successful Load or Assemble: defaults
	// are used when no Settings document is present.
	Settings *Settings
	// Warnings are non-fatal findings, such as provider overrides.
	Warnings Diagnostics
}

func (c *Config) add(doc envelope) {
	switch v := doc.(type) {
	case *Provider:
		c.Providers = append(c.Providers, v)
	case *JobTemplate:
		c.JobTemplates = append(c.JobTemplates, v)
	case *Account:
		c.Accounts = append(c.Accounts, v)
	case *Job:
		c.Jobs = append(c.Jobs, v)
	case *Output:
		c.Outputs = append(c.Outputs, v)
	case *Settings:
		c.Settings = v
	}
}

// Loader loads configuration documents. It is safe for concurrent use.
type Loader struct {
	schemas map[Kind]*jsonschema.Schema
}

// NewLoader compiles the embedded JSON Schemas.
func NewLoader() (*Loader, error) {
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	l := &Loader{schemas: make(map[Kind]*jsonschema.Schema, len(allKinds))}
	for _, k := range allKinds {
		b, err := fs.ReadFile(schemas.V1alpha1, "v1alpha1/"+SchemaFile(k))
		if err != nil {
			return nil, fmt.Errorf("reading embedded %s schema: %w", k, err)
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
		if err != nil {
			return nil, fmt.Errorf("parsing embedded %s schema: %w", k, err)
		}
		url := SchemaBaseURL + SchemaFile(k)
		if err := c.AddResource(url, doc); err != nil {
			return nil, fmt.Errorf("adding %s schema: %w", k, err)
		}
		if l.schemas[k], err = c.Compile(url); err != nil {
			return nil, fmt.Errorf("compiling %s schema: %w", k, err)
		}
	}
	return l, nil
}

// rawDocument is a parsed document before decoding.
type rawDocument struct {
	kind  Kind
	tree  map[string]any
	src   *Source
	index *positionIndex
}

// userKinds are the kinds accepted in configuration paths.
var userKinds = func() map[Kind]bool {
	m := make(map[Kind]bool, len(kinds))
	for _, k := range kinds {
		m[k] = true
	}
	return m
}()

// Load reads every path (files, or directories scanned recursively for
// *.yaml and *.yml files), validates the documents and returns the
// configuration, without provider packages. Errors are returned as
// Diagnostics, all at once.
func (l *Loader) Load(paths []string) (*Config, error) {
	cfg, err := l.Parse(paths)
	if err != nil {
		return nil, err
	}
	return Assemble(cfg, nil)
}

// Parse reads and decodes the documents of paths, validating each one
// against its schema and resolving relative paths, but without
// cross-document validation. Use Assemble to complete the configuration.
func (l *Loader) Parse(paths []string) (*Config, error) {
	if len(paths) == 0 {
		return nil, errors.New("no configuration path given (use --config or TAPYR_CONFIG)")
	}
	files, diags := collectFiles(paths)
	cfg := &Config{}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			diags = append(diags, Diagnostic{Pos: Position{File: f}, Message: fileError(err)})
			continue
		}
		docs, d := l.parseDocuments(f, data, userKinds)
		diags = append(diags, d...)
		for _, doc := range docs {
			if s, ok := doc.(*Settings); ok && cfg.Settings != nil {
				diags = append(diags, s.Source.Diagnostic(fmt.Sprintf("at most one Settings document is allowed (another one is at %s)",
					cfg.Settings.Source.Position("kind")), "kind"))
				continue
			}
			cfg.add(doc)
		}
	}
	if len(diags) > 0 {
		return nil, diags.Sorted()
	}
	resolvePaths(cfg)
	return cfg, nil
}

// Assemble merges provider packages into the documents of cfg, then runs
// semantic validation and applies defaults. Packages are given in
// precedence order (user packages first, built-in packages last); providers
// defined directly in cfg win over every package. Overridden packages are
// dropped entirely and reported in cfg.Warnings.
func Assemble(cfg *Config, packages []*Package) (*Config, error) {
	origin := map[string]string{}
	for _, p := range cfg.Providers {
		origin[p.name()] = "the configuration (" + p.Source.Position("metadata", "name").String() + ")"
	}
	for _, pkg := range packages {
		name := pkg.Provider.name()
		if winner, ok := origin[name]; ok {
			cfg.Warnings = append(cfg.Warnings, pkg.Provider.Source.Diagnostic(
				fmt.Sprintf("provider %q from %s is overridden by %s", name, pkg.Origin, winner), "metadata", "name"))
			continue
		}
		origin[name] = pkg.Origin
		cfg.Providers = append(cfg.Providers, pkg.Provider)
		cfg.JobTemplates = append(cfg.JobTemplates, pkg.JobTemplates...)
	}
	diags, warnings := validateSemantics(cfg)
	if len(diags) > 0 {
		return nil, diags.Sorted()
	}
	cfg.Warnings = append(cfg.Warnings, warnings...)
	applyDefaults(cfg)
	cfg.Warnings = cfg.Warnings.Sorted()
	return cfg, nil
}

// collectFiles expands paths into a sorted, de-duplicated list of files.
func collectFiles(paths []string) ([]string, Diagnostics) {
	var files []string
	var diags Diagnostics
	seen := map[string]bool{}
	add := func(p string) {
		abs, err := filepath.Abs(p)
		if err != nil {
			abs = p
		}
		if !seen[abs] {
			seen[abs] = true
			files = append(files, p)
		}
	}
	for _, root := range paths {
		info, err := os.Stat(root)
		if err != nil {
			diags = append(diags, Diagnostic{Pos: Position{File: root}, Message: fileError(err)})
			continue
		}
		if !info.IsDir() {
			add(root)
			continue
		}
		var found []string
		err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				diags = append(diags, Diagnostic{Pos: Position{File: p}, Message: fileError(err)})
				return nil
			}
			if p != root && strings.HasPrefix(d.Name(), ".") {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if !d.IsDir() && isYAML(p) {
				found = append(found, p)
			}
			return nil
		})
		if err != nil {
			diags = append(diags, Diagnostic{Pos: Position{File: root}, Message: fileError(err)})
		}
		slices.Sort(found)
		for _, f := range found {
			add(f)
		}
	}
	return files, diags
}

func isYAML(name string) bool {
	return strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml")
}

func fileError(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		err = pe.Err
	}
	return err.Error()
}

// parseDocuments splits a file into documents, validates and decodes them.
// Only the kinds in allowed are accepted.
func (l *Loader) parseDocuments(file string, data []byte, allowed map[Kind]bool) ([]envelope, Diagnostics) {
	f, err := parser.ParseBytes(data, 0)
	if err != nil {
		return nil, Diagnostics{yamlDiagnostic(file, err)}
	}
	var docs []envelope
	var diags Diagnostics
	for i, doc := range f.Docs {
		if doc.Body == nil {
			continue
		}
		b := newTreeBuilder(file)
		v, err := b.convert(doc.Body, nil, nil, false)
		if err != nil {
			diags = append(diags, treeDiagnostic(file, err))
			continue
		}
		tree, ok := v.(map[string]any)
		if !ok {
			diags = append(diags, Diagnostic{Pos: tokenPos(file, doc.Body), Message: "a document must be a mapping"})
			continue
		}
		src := &Source{File: file, Index: i, positions: b.index}
		k, d := envelopeKind(tree, b.index, allowed)
		if d != nil {
			diags = append(diags, *d)
			continue
		}
		decoded, d2 := l.decode(&rawDocument{kind: k, tree: tree, src: src, index: b.index})
		diags = append(diags, d2...)
		if decoded != nil {
			docs = append(docs, decoded)
		}
	}
	return docs, diags
}

func yamlDiagnostic(file string, err error) Diagnostic {
	var ye yaml.Error
	if errors.As(err, &ye) && ye.GetToken() != nil {
		p := ye.GetToken().Position
		return Diagnostic{Pos: Position{File: file, Line: p.Line, Column: p.Column}, Message: "invalid YAML: " + ye.GetMessage()}
	}
	return Diagnostic{Pos: Position{File: file}, Message: "invalid YAML: " + err.Error()}
}

func treeDiagnostic(file string, err error) Diagnostic {
	if te, ok := asTreeError(err); ok && te.tok != nil {
		return Diagnostic{Pos: Position{File: file, Line: te.tok.Position.Line, Column: te.tok.Position.Column}, Message: te.msg}
	}
	return Diagnostic{Pos: Position{File: file}, Message: err.Error()}
}

func tokenPos(file string, n ast.Node) Position {
	if tk := n.GetToken(); tk != nil {
		return Position{File: file, Line: tk.Position.Line, Column: tk.Position.Column}
	}
	return Position{File: file}
}

// envelopeKind checks apiVersion and kind before schema validation, so that
// the right schema is used and envelope errors are explicit.
func envelopeKind(tree map[string]any, idx *positionIndex, allowed map[Kind]bool) (Kind, *Diagnostic) {
	at := func(field, msg string) *Diagnostic {
		tokens := []string{field}
		if _, ok := tree[field]; !ok {
			tokens = nil
		}
		return &Diagnostic{Pos: idx.position(tokens, false), Path: idx.path(tokens), Message: msg}
	}
	v, ok := tree["apiVersion"]
	if !ok {
		return "", at("apiVersion", "missing required field \"apiVersion\" (want "+APIVersion+")")
	}
	if v != APIVersion {
		return "", at("apiVersion", fmt.Sprintf("unsupported apiVersion %s (want %s)", quoteAny(v), APIVersion))
	}
	kv, ok := tree["kind"]
	if !ok {
		return "", at("kind", "missing required field \"kind\"")
	}
	ks, _ := kv.(string)
	k := Kind(ks)
	switch {
	case allowed[k]:
		return k, nil
	case packageKinds[k] && allowed[KindProvider]:
		return "", at("kind", fmt.Sprintf("kind %s is only valid inside a provider package (sim/ directory)", ks))
	case documentTypes[k] != nil:
		return "", at("kind", fmt.Sprintf("kind %s is not allowed here (want %s)", ks, joinKinds(allowed)))
	}
	return "", at("kind", fmt.Sprintf("unknown kind %s (want one of %s)", quoteAny(kv), joinKinds(allowed)))
}

func joinKinds(allowed map[Kind]bool) string {
	var names []string
	for _, k := range allKinds {
		if allowed[k] {
			names = append(names, string(k))
		}
	}
	return strings.Join(names, ", ")
}

// decode validates a document against its schema and decodes it.
func (l *Loader) decode(d *rawDocument) (envelope, Diagnostics) {
	if err := l.schemas[d.kind].Validate(any(d.tree)); err != nil {
		var ve *jsonschema.ValidationError
		if errors.As(err, &ve) {
			return nil, schemaDiagnostics(ve, d.index)
		}
		return nil, Diagnostics{{Pos: d.index.position(nil, false), Message: err.Error()}}
	}
	b, err := json.Marshal(d.tree)
	if err != nil {
		return nil, Diagnostics{{Pos: d.index.position(nil, false), Message: err.Error()}}
	}
	doc := reflect.New(documentTypes[d.kind])
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(doc.Interface()); err != nil {
		// The schema should reject anything the decoder rejects; report it
		// at the document level if it does not.
		return nil, Diagnostics{{Pos: d.index.position(nil, false), Message: "invalid document: " + err.Error()}}
	}
	doc.Elem().FieldByName("Source").Set(reflect.ValueOf(d.src))
	return doc.Interface().(envelope), nil
}

func quoteAny(v any) string {
	if s, ok := v.(string); ok {
		return fmt.Sprintf("%q", s)
	}
	b, _ := json.Marshal(v)
	return string(b)
}
