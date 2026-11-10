package config

import (
	"strings"

	"github.com/invopop/jsonschema"
)

// Provider describes an API family once: base URL, auth, pagination, error
// semantics and every rate limit. Written by provider package authors.
type Provider struct {
	APIVersion string       `json:"apiVersion" jsonschema:"required"`
	Kind       string       `json:"kind" jsonschema:"required"`
	Metadata   ProviderMeta `json:"metadata" jsonschema:"required"`
	Spec       ProviderSpec `json:"spec" jsonschema:"required"`

	Source *Source `json:"-"`
}

// JSONSchemaExtend implements the schema reflector hook.
func (Provider) JSONSchemaExtend(s *jsonschema.Schema) { extendEnvelope(s, KindProvider) }

func (p *Provider) kind() Kind      { return KindProvider }
func (p *Provider) name() string    { return string(p.Metadata.Name) }
func (p *Provider) source() *Source { return p.Source }

// ProviderSpec is the specification of a Provider.
type ProviderSpec struct {
	// BaseURL of the API. Template evaluated per account and target, for
	// example {{ .params.baseURL | default "https://api.example.com/v1" }}.
	BaseURL Template `json:"baseURL" jsonschema:"required"`
	// Params an Account may or must provide.
	Params []ParamSpec `json:"params,omitempty"`
	// Secrets an Account may or must provide.
	Secrets []ParamSpec `json:"secrets,omitempty"`
	// Auth applied to every request. Defaults to type none.
	Auth *Auth `json:"auth,omitempty"`
	// Defaults applied to every request.
	Defaults *RequestDefaults `json:"defaults,omitempty"`
	// Pagination used by job templates that do not override it.
	Pagination *Pagination `json:"pagination,omitempty"`
	// Limits declares every rate limit, concurrency limit and quota of the API.
	Limits []Limit `json:"limits,omitempty"`
	// Errors declares how the API signals rate limiting, authentication
	// failures and retryable errors.
	Errors *ErrorSemantics `json:"errors,omitempty"`
	// Redirects lists the other hosts a response may redirect requests to.
	// Redirects to another host are not followed otherwise: credentials
	// only go to the base URL and to these hosts.
	Redirects *Redirects `json:"redirects,omitempty"`
}

// Redirects allows HTTP redirects to other hosts. Every redirect followed
// is a request of its own, reserved in the budget engine and sent with the
// credentials of the account; redirects within the origin of the request
// are always followed, up to 5 in a row.
type Redirects struct {
	// Hosts the requests may be redirected to: a host name, or "*." and a
	// domain for every host under it (for example "*.meraki.com").
	Hosts []string `json:"hosts" jsonschema:"required,minItems=1"`
}

// ParamSpec declares a param or a secret that accounts provide.
type ParamSpec struct {
	// Name of the param or secret.
	Name Identifier `json:"name" jsonschema:"required"`
	// Required makes the param or secret mandatory for every account (or
	// every target of a multi-target account).
	Required bool `json:"required,omitempty"`
	// Description is free text for humans.
	Description string `json:"description,omitempty"`
}

// RequestDefaults are applied to every request of the provider.
type RequestDefaults struct {
	// Headers added to every request. Values are templates.
	Headers map[string]Template `json:"headers,omitempty"`
	// Timeout of a single request. Defaults to settings.collection.defaultTimeout.
	Timeout Duration `json:"timeout,omitempty"`
}

// AuthType selects an authentication mechanism.
type AuthType string

// Auth types.
//
//nolint:gosec // G101: auth type names, not credentials.
const (
	AuthNone                    AuthType = "none"
	AuthHeader                  AuthType = "header"
	AuthBearer                  AuthType = "bearer"
	AuthBasic                   AuthType = "basic"
	AuthOAuth2ClientCredentials AuthType = "oauth2_client_credentials"
	AuthSessionLogin            AuthType = "session_login"
	AuthPlugin                  AuthType = "plugin"
)

// Auth configures authentication. The block matching the type must be set,
// named after the type in camelCase.
type Auth struct {
	// Type of authentication.
	Type AuthType `json:"type" jsonschema:"required,enum=none,enum=header,enum=bearer,enum=basic,enum=oauth2_client_credentials,enum=session_login,enum=plugin"`
	// Header sends a static value in a header (type header).
	Header *HeaderAuth `json:"header,omitempty"`
	// Bearer sends Authorization: Bearer <token> (type bearer).
	Bearer *BearerAuth `json:"bearer,omitempty"`
	// Basic sends HTTP basic credentials (type basic).
	Basic *BasicAuth `json:"basic,omitempty"`
	// OAuth2ClientCredentials obtains tokens with the OAuth 2.0 client
	// credentials grant (type oauth2_client_credentials).
	OAuth2ClientCredentials *OAuth2ClientCredentials `json:"oauth2ClientCredentials,omitempty"`
	// SessionLogin posts credentials to obtain a session token (type session_login).
	SessionLogin *SessionLogin `json:"sessionLogin,omitempty"`
	// Plugin delegates to a Go auth plugin (type plugin).
	Plugin *PluginAuth `json:"plugin,omitempty"`
}

// authBlocks maps each auth type to the name of its configuration block.
//
//nolint:gosec // G101: block names, not credentials.
var authBlocks = map[AuthType]string{
	AuthHeader:                  "header",
	AuthBearer:                  "bearer",
	AuthBasic:                   "basic",
	AuthOAuth2ClientCredentials: "oauth2ClientCredentials",
	AuthSessionLogin:            "sessionLogin",
	AuthPlugin:                  "plugin",
}

// JSONSchemaExtend implements the schema reflector hook.
func (Auth) JSONSchemaExtend(s *jsonschema.Schema) {
	for _, t := range sortedKeys(authBlocks) {
		requireWhen(s, "type", string(t), authBlocks[t])
	}
}

// HeaderAuth sends a static credential in a request header.
type HeaderAuth struct {
	// Name of the header, for example X-API-Key.
	Name string `json:"name" jsonschema:"required,minLength=1"`
	// Value of the header, usually {secret: name}.
	Value Value `json:"value" jsonschema:"required"`
	// Format of the header value; {value} is replaced by the value, for
	// example "Token {value}".
	Format string `json:"format,omitempty" jsonschema:"default={value}"`
}

// Formatted returns the header value for value, with Format applied.
func (h *HeaderAuth) Formatted(value string) string {
	if h.Format == "" {
		return value
	}
	return strings.ReplaceAll(h.Format, "{value}", value)
}

// BearerAuth sends Authorization: Bearer <token>.
type BearerAuth struct {
	// Token, usually {secret: name}.
	Token Value `json:"token" jsonschema:"required"`
}

// BasicAuth sends HTTP basic credentials.
type BasicAuth struct {
	// Username, a literal or {secret: name}.
	Username Value `json:"username" jsonschema:"required"`
	// Password, usually {secret: name}.
	Password Value `json:"password" jsonschema:"required"`
}

// OAuth2ClientCredentials configures the OAuth 2.0 client credentials grant.
// Tokens are cached until expires_in minus a safety margin and persisted.
type OAuth2ClientCredentials struct {
	// TokenURL of the authorization server. Template.
	TokenURL Template `json:"tokenURL" jsonschema:"required"`
	// ClientID, a literal or {secret: name}.
	ClientID Value `json:"clientId" jsonschema:"required"`
	// ClientSecret, usually {secret: name}.
	ClientSecret Value `json:"clientSecret" jsonschema:"required"`
	// Scopes requested.
	Scopes []string `json:"scopes,omitempty"`
	// Audience requested, when the server requires one.
	Audience string `json:"audience,omitempty"`
	// ExtraParams added to the token request.
	ExtraParams map[string]string `json:"extraParams,omitempty"`
	// AuthStyle sends the client credentials in the Authorization header or
	// in the request body.
	AuthStyle string `json:"authStyle,omitempty" jsonschema:"enum=header,enum=body,default=header"`
}

// SessionLogin posts credentials to a login endpoint and applies the
// returned token to subsequent requests.
type SessionLogin struct {
	// Request is the login request.
	Request LoginRequest `json:"request" jsonschema:"required"`
	// Token is a CEL expression extracting the token from the login response.
	Token Expr `json:"token" jsonschema:"required"`
	// ExpiresIn is a CEL expression returning the token lifetime as a duration.
	ExpiresIn Expr `json:"expiresIn,omitempty"`
	// Apply describes how the token is attached to requests.
	Apply TokenApply `json:"apply" jsonschema:"required"`
	// RefreshBefore renews the session this long before it expires.
	RefreshBefore Duration `json:"refreshBefore,omitempty" jsonschema:"default=30s"`
	// PerTarget keeps one session per target of a multi-target account.
	PerTarget bool `json:"perTarget,omitempty"`
	// Logout closes a session when its token is replaced and when Tapyr
	// stops, for APIs that keep sessions open until they are closed.
	Logout *LogoutRequest `json:"logout,omitempty"`
}

// LogoutRequest is the request that closes a session, sent with the token
// applied.
type LogoutRequest struct {
	// Method of the logout request.
	Method string `json:"method,omitempty" jsonschema:"enum=POST,enum=GET,enum=DELETE,default=POST"`
	// Path of the logout endpoint, relative to the base URL.
	Path string `json:"path" jsonschema:"required,pattern=^/"`
}

// LoginRequest is the request that opens a session.
type LoginRequest struct {
	// Method of the login request.
	Method string `json:"method,omitempty" jsonschema:"enum=POST,enum=GET,default=POST"`
	// Path of the login endpoint, relative to the base URL.
	Path string `json:"path" jsonschema:"required,pattern=^/"`
	// Headers added to the login request.
	Headers map[string]string `json:"headers,omitempty"`
	// Body is a JSON object; values are literals, secrets or CEL expressions.
	Body map[string]Value `json:"body,omitempty"`
}

// TokenApply attaches a session token to requests, as a header or a cookie.
type TokenApply struct {
	// Header that carries the token.
	Header string `json:"header,omitempty"`
	// Format of the header value; {token} is replaced by the token.
	Format string `json:"format,omitempty" jsonschema:"default={token}"`
	// Cookie that carries the token.
	Cookie string `json:"cookie,omitempty"`
}

// PluginAuth selects a Go auth plugin, for flows that cannot be expressed
// declaratively.
type PluginAuth struct {
	// Name of the plugin, as registered in internal/auth/plugins.
	Name string `json:"name" jsonschema:"required,pattern=^[a-z][a-z0-9_]*$"`
	// Config is passed to the plugin, which validates it.
	Config map[string]any `json:"config,omitempty"`
}

// ErrorSemantics declares how the provider signals errors. Omitted parts use
// the defaults described in docs/spec/03-configuration.md.
type ErrorSemantics struct {
	// RateLimited matches rate-limited responses.
	RateLimited *RateLimitedErrors `json:"rateLimited,omitempty"`
	// AuthFailed matches authentication failures (default 401).
	AuthFailed *StatusMatcher `json:"authFailed,omitempty"`
	// Retryable matches transient errors (default 500, 502, 503, 504).
	Retryable *StatusMatcher `json:"retryable,omitempty"`
}

// StatusMatcher matches responses by HTTP status code.
type StatusMatcher struct {
	// Statuses that match.
	Statuses []int `json:"statuses,omitempty" jsonschema:"minItems=1"`
}

// JSONSchemaExtend implements the schema reflector hook.
func (StatusMatcher) JSONSchemaExtend(s *jsonschema.Schema) { constrainStatuses(s) }

// RateLimitedErrors matches rate-limited responses and configures penalties.
type RateLimitedErrors struct {
	// Statuses that signal rate limiting (default 429).
	Statuses []int `json:"statuses,omitempty" jsonschema:"minItems=1"`
	// RetryAfterHeader carries the penalty in seconds or as an HTTP date.
	RetryAfterHeader string `json:"retryAfterHeader,omitempty" jsonschema:"default=Retry-After"`
	// DefaultPenalty applies when no Retry-After header is present. It grows
	// exponentially on consecutive rate-limited responses, up to 10 minutes.
	DefaultPenalty Duration `json:"defaultPenalty,omitempty" jsonschema:"default=2s"`
	// Penalize the narrowest applicable scope, or all applicable scopes.
	Penalize string `json:"penalize,omitempty" jsonschema:"enum=narrowest,enum=all,default=narrowest"`
}

// JSONSchemaExtend implements the schema reflector hook.
func (RateLimitedErrors) JSONSchemaExtend(s *jsonschema.Schema) { constrainStatuses(s) }

func constrainStatuses(s *jsonschema.Schema) {
	if p, ok := s.Properties.Get("statuses"); ok && p.Items != nil {
		p.Items.Minimum = "100"
		p.Items.Maximum = "599"
	}
}
