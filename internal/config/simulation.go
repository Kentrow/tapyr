package config

import "github.com/invopop/jsonschema"

// Simulation describes the simulator of a provider package (sim/sim.yaml).
// The simulator also reads the package's Provider: limits, auth and
// pagination are declared once (see docs/spec/08-simulator.md).
type Simulation struct {
	APIVersion string         `json:"apiVersion" jsonschema:"required"`
	Kind       string         `json:"kind" jsonschema:"required"`
	Metadata   ObjectMeta     `json:"metadata" jsonschema:"required"`
	Spec       SimulationSpec `json:"spec" jsonschema:"required"`

	Source *Source `json:"-"`
}

// JSONSchemaExtend implements the schema reflector hook.
func (Simulation) JSONSchemaExtend(s *jsonschema.Schema) { extendEnvelope(s, KindSimulation) }

func (s *Simulation) kind() Kind      { return KindSimulation }
func (s *Simulation) name() string    { return string(s.Metadata.Name) }
func (s *Simulation) source() *Source { return s.Source }

// SimulationSpec is the specification of a Simulation.
type SimulationSpec struct {
	// BasePath is the path prefix of the API (the path part of the provider
	// base URL). Routes are relative to it.
	BasePath string `json:"basePath,omitempty" jsonschema:"pattern=^/,default=/"`
	// PortOffset is the port of the simulator in tapyr sim --all, as an
	// offset from --listen-base, so that adding a package does not move the
	// others. Simulators without one take the ports after the largest
	// declared offset, in provider name order.
	PortOffset *int `json:"portOffset,omitempty" jsonschema:"minimum=0"`
	// Auth lists the credentials the simulator accepts. The way they are
	// checked follows the provider auth type.
	Auth *SimulationAuth `json:"auth,omitempty"`
	// Inventory generates synthetic items for templated fixtures.
	Inventory *SimulationInventory `json:"inventory,omitempty"`
	// Routes served by the simulator. Unknown routes return 404.
	Routes []SimulationRoute `json:"routes" jsonschema:"required,minItems=1"`
	// Headers added to every response, computed from the limit state:
	// "limit:<name>.max", "limit:<name>.remaining" or "limit:<name>.reset"
	// (seconds until the window resets).
	Headers map[string]string `json:"headers,omitempty"`
}

// SimulationInventory is a synthetic inventory: sets of items, each item
// under a parent item of another set.
type SimulationInventory struct {
	// Seed of the generated values. The same seed gives the same items and
	// the same dynamics.
	Seed uint64 `json:"seed,omitempty" jsonschema:"default=42"`
	// Sets of items, generated in order: a parent set comes first.
	Sets []SimulationSet `json:"sets" jsonschema:"required,minItems=1"`
}

// SimulationSet is a set of generated items.
type SimulationSet struct {
	// Name of the set, used by the template helpers.
	Name Name `json:"name" jsonschema:"required"`
	// Parent set: count items are generated under each of its items.
	Parent Name `json:"parent,omitempty"`
	// Count of items (per parent item when Parent is set).
	Count int `json:"count" jsonschema:"required,minimum=0,maximum=100000"`
}

// SimulationAuth configures the credentials accepted by the simulator.
type SimulationAuth struct {
	// Accept lists the accepted credentials. They are fake values by design.
	Accept SimulationCredentials `json:"accept"`
	// TokenLifetime of the tokens issued by the token and login endpoints,
	// in simulated time.
	TokenLifetime Duration `json:"tokenLifetime,omitempty" jsonschema:"default=10m"`
	// LoginResponse is a JSON fixture answered by the login endpoint, relative
	// to the sim/ directory; {token} and {expiresIn} (seconds) are replaced.
	// Default: {"token": "{token}", "expiresIn": {expiresIn}}.
	LoginResponse string `json:"loginResponse,omitempty"`
}

// SimulationCredentials are accepted credentials per auth type.
type SimulationCredentials struct {
	// Bearer tokens accepted with auth type bearer.
	Bearer []string `json:"bearer,omitempty"`
	// Header values accepted with auth type header, before the format of
	// the provider auth is applied.
	Header []string `json:"header,omitempty"`
	// Basic credentials accepted with auth type basic.
	Basic []SimulationBasicCredential `json:"basic,omitempty"`
	// Clients accepted by the token endpoint with auth type
	// oauth2_client_credentials.
	Clients []SimulationClient `json:"clients,omitempty"`
	// Logins accepted by the login endpoint with auth type session_login:
	// the login body must carry both values.
	Logins []SimulationBasicCredential `json:"logins,omitempty"`
	// Plugin holds the credentials accepted by the simulator side of an
	// auth plugin (type plugin), read by the plugin.
	Plugin map[string]any `json:"plugin,omitempty"`
}

// SimulationClient is an OAuth 2.0 client.
type SimulationClient struct {
	// ClientID accepted.
	ClientID string `json:"clientId" jsonschema:"required"`
	// ClientSecret accepted.
	ClientSecret string `json:"clientSecret" jsonschema:"required"`
}

// SimulationBasicCredential is a username and password pair.
type SimulationBasicCredential struct {
	// Username accepted.
	Username string `json:"username" jsonschema:"required"`
	// Password accepted.
	Password string `json:"password" jsonschema:"required"`
}

// SimulationRoute is an endpoint of the simulator.
type SimulationRoute struct {
	// Method of the route.
	Method string `json:"method,omitempty" jsonschema:"enum=GET,enum=HEAD,enum=POST,default=GET"`
	// Path of the route, relative to basePath, with {var} segments.
	Path string `json:"path" jsonschema:"required,pattern=^/"`
	// Pagination serves the fixture items page by page. Its parameters come
	// from the job templates requesting this path, or from the provider.
	Pagination PaginationType `json:"pagination,omitempty" jsonschema:"enum=none,enum=offset,enum=page,enum=link_header,enum=cursor,enum=expression,default=none"`
	// Response returned by the route.
	Response SimulationResponse `json:"response" jsonschema:"required"`
}

// SimulationResponse is the response of a route.
type SimulationResponse struct {
	// Status code of the response.
	Status int `json:"status,omitempty" jsonschema:"minimum=100,maximum=599,default=200"`
	// Headers added to the response.
	Headers map[string]string `json:"headers,omitempty"`
	// File is the JSON fixture, relative to the sim/ directory. {var}
	// placeholders are replaced by the route variables. Paginated routes
	// need a JSON array, or a list at ItemsField.
	File string `json:"file,omitempty" jsonschema:"minLength=1"`
	// Template is a Go text/template fixture rendering JSON, relative to
	// the sim/ directory (see docs/spec/08-simulator.md for its data and
	// helpers). Exactly one of file and template is set.
	Template string `json:"template,omitempty" jsonschema:"minLength=1"`
	// ItemsField is the dotted path of the list a paginated route serves
	// page by page, inside an envelope; empty for a top-level array.
	ItemsField string `json:"itemsField,omitempty"`
}

// JSONSchemaExtend implements the schema reflector hook.
func (SimulationResponse) JSONSchemaExtend(s *jsonschema.Schema) {
	s.OneOf = []*jsonschema.Schema{{Required: []string{"file"}}, {Required: []string{"template"}}}
}

// Scenario overlays faults on the default behavior of a simulator, on a
// timeline (sim/scenarios/*.yaml, see docs/spec/08-simulator.md).
type Scenario struct {
	APIVersion string       `json:"apiVersion" jsonschema:"required"`
	Kind       string       `json:"kind" jsonschema:"required"`
	Metadata   ObjectMeta   `json:"metadata" jsonschema:"required"`
	Spec       ScenarioSpec `json:"spec" jsonschema:"required"`

	Source *Source `json:"-"`
}

// JSONSchemaExtend implements the schema reflector hook.
func (Scenario) JSONSchemaExtend(s *jsonschema.Schema) { extendEnvelope(s, KindScenario) }

func (s *Scenario) kind() Kind      { return KindScenario }
func (s *Scenario) name() string    { return string(s.Metadata.Name) }
func (s *Scenario) source() *Source { return s.Source }

// ScenarioSpec is the specification of a Scenario.
type ScenarioSpec struct {
	// Steps of the timeline.
	Steps []ScenarioStep `json:"steps,omitempty"`
}

// ScenarioStep applies from at until at + for (or until the end), in
// simulated time since the simulator start.
type ScenarioStep struct {
	// At is the start of the step.
	At Duration `json:"at,omitempty"`
	// For is the duration of the step; empty means until the end.
	For Duration `json:"for,omitempty"`
	// Limits overrides the max of named quota limits.
	Limits map[string]ScenarioLimit `json:"limits,omitempty"`
	// ExternalConsumption simulates another client consuming a quota.
	ExternalConsumption *ExternalConsumption `json:"externalConsumption,omitempty"`
	// Errors answers a ratio of the requests to matching routes with a status.
	Errors *ScenarioErrors `json:"errors,omitempty"`
	// Fixtures replaces the fixture of routes, by route path, to simulate
	// inventory changes. Files are relative to the sim/ directory and take
	// the {var} placeholders of the route.
	Fixtures map[string]string `json:"fixtures,omitempty"`
	// Latency delays the responses (log-normal, in simulated time).
	Latency *ScenarioLatency `json:"latency,omitempty"`
	// Outage closes connections without a response.
	Outage bool `json:"outage,omitempty"`
	// TokenExpiry invalidates, when the step starts, the tokens issued
	// before it.
	TokenExpiry bool `json:"tokenExpiry,omitempty"`
}

// ScenarioLatency is a log-normal response delay given by two quantiles.
type ScenarioLatency struct {
	// P50 is the median delay.
	P50 Duration `json:"p50" jsonschema:"required"`
	// P99 is the 99th percentile, at least P50.
	P99 Duration `json:"p99,omitempty"`
}

// ScenarioLimit overrides a quota limit.
type ScenarioLimit struct {
	// Max of the quota per window.
	Max int64 `json:"max" jsonschema:"required,minimum=0"`
}

// ExternalConsumption consumes a quota at a steady rate.
type ExternalConsumption struct {
	// Limit is the name of a quota limit of the provider.
	Limit Name `json:"limit" jsonschema:"required"`
	// RatePerHour is the consumption rate.
	RatePerHour float64 `json:"ratePerHour" jsonschema:"required,minimum=0"`
}

// ScenarioErrors injects error responses.
type ScenarioErrors struct {
	// Routes are globs on the route paths (* within a segment, ** across
	// segments). Empty means every route.
	Routes []string `json:"routes,omitempty"`
	// Status of the injected responses.
	Status int `json:"status" jsonschema:"required,minimum=400,maximum=599"`
	// Ratio of the matching requests that fail (0 to 1).
	Ratio float64 `json:"ratio" jsonschema:"required,minimum=0,maximum=1"`
}
