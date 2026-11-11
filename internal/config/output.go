package config

import "github.com/invopop/jsonschema"

// Output sends collected samples to a backend.
type Output struct {
	APIVersion string     `json:"apiVersion" jsonschema:"required"`
	Kind       string     `json:"kind" jsonschema:"required"`
	Metadata   ObjectMeta `json:"metadata" jsonschema:"required"`
	Spec       OutputSpec `json:"spec" jsonschema:"required"`

	Source *Source `json:"-"`
}

// JSONSchemaExtend implements the schema reflector hook.
func (Output) JSONSchemaExtend(s *jsonschema.Schema) { extendEnvelope(s, KindOutput) }

func (o *Output) kind() Kind      { return KindOutput }
func (o *Output) name() string    { return string(o.Metadata.Name) }
func (o *Output) source() *Source { return o.Source }

// OutputType selects the output protocol.
type OutputType string

// Output types.
const (
	OutputPrometheus  OutputType = "prometheus"
	OutputRemoteWrite OutputType = "remote_write"
	OutputOTLP        OutputType = "otlp"
)

// outputBlocks maps each output type to its configuration block.
var outputBlocks = map[OutputType]string{
	OutputPrometheus:  "prometheus",
	OutputRemoteWrite: "remoteWrite",
	OutputOTLP:        "otlp",
}

// OutputSpec is the specification of an Output.
type OutputSpec struct {
	// Type of output.
	Type OutputType `json:"type" jsonschema:"required,enum=prometheus,enum=remote_write,enum=otlp"`
	// Prometheus pull exposition on /metrics (type prometheus, at most one).
	Prometheus *PrometheusOutput `json:"prometheus,omitempty"`
	// RemoteWrite pushes with Prometheus remote write 1.0 (type remote_write).
	RemoteWrite *RemoteWriteOutput `json:"remoteWrite,omitempty"`
	// OTLP pushes OTLP metrics (type otlp).
	OTLP *OTLPOutput `json:"otlp,omitempty"`
	// Queue of a push output (not allowed for prometheus).
	Queue *Queue `json:"queue,omitempty"`
	// ExternalLabels added to every sample sent through this output.
	ExternalLabels map[string]string `json:"externalLabels,omitempty"`
	// Processing is an ordered list of rules applied to the samples of the
	// jobs sent to this output only, with the semantics of Prometheus
	// metric_relabel_configs (ADR 0012). Self-metrics are never processed.
	Processing []ProcessingRule `json:"processing,omitempty"`
}

// JSONSchemaExtend implements the schema reflector hook.
func (ProcessingRule) JSONSchemaExtend(s *jsonschema.Schema) {
	if p, ok := s.Properties.Get("sourceLabels"); ok && p.Items != nil {
		p.Items.Pattern = labelNamePattern
	}
}

// ProcessingAction is the action of a processing rule.
type ProcessingAction string

// Processing actions.
const (
	ProcessingKeep      ProcessingAction = "keep"
	ProcessingDrop      ProcessingAction = "drop"
	ProcessingLabelKeep ProcessingAction = "labelkeep"
	ProcessingLabelDrop ProcessingAction = "labeldrop"
)

// ProcessingRule keeps or drops samples, or labels, of an output.
type ProcessingRule struct {
	// Action: keep or drop the samples whose joined sourceLabels match
	// regex; labelkeep or labeldrop the labels whose name matches regex.
	Action ProcessingAction `json:"action" jsonschema:"required,enum=keep,enum=drop,enum=labelkeep,enum=labeldrop"`
	// SourceLabels are the labels whose values are joined and matched
	// (keep and drop, required); __name__ is the metric name, a missing
	// label the empty string.
	SourceLabels []string `json:"sourceLabels,omitempty"`
	// Separator joins the values of sourceLabels (keep and drop).
	Separator *string `json:"separator,omitempty" jsonschema:"default=;"`
	// Regex is an RE2 regular expression, fully anchored. keep and drop
	// default to (.*); labelkeep and labeldrop require it.
	Regex string `json:"regex,omitempty"`
}

// JSONSchemaExtend implements the schema reflector hook.
func (OutputSpec) JSONSchemaExtend(s *jsonschema.Schema) {
	requireWhen(s, "type", string(OutputRemoteWrite), "remoteWrite")
	requireWhen(s, "type", string(OutputOTLP), "otlp")
	if p, ok := s.Properties.Get("externalLabels"); ok {
		p.PropertyNames = &jsonschema.Schema{Pattern: labelNamePattern}
	}
}

// PrometheusOutput configures the pull exposition.
type PrometheusOutput struct {
	// ExposeTimestamps adds sample timestamps to the exposition. Off by
	// default, as recommended by Prometheus.
	ExposeTimestamps bool `json:"exposeTimestamps,omitempty"`
}

// RemoteWriteOutput configures Prometheus remote write 1.0.
type RemoteWriteOutput struct {
	// URL of the remote write endpoint.
	URL string `json:"url" jsonschema:"required,format=uri"`
	// Headers added to every request; values are literals or secrets.
	Headers map[string]StringOrSecret `json:"headers,omitempty"`
	// BasicAuth credentials.
	BasicAuth *BasicAuthSecrets `json:"basicAuth,omitempty"`
	// Timeout of a send request.
	Timeout Duration `json:"timeout,omitempty" jsonschema:"default=10s"`
}

// OTLPOutput configures OTLP metrics export.
type OTLPOutput struct {
	// Endpoint of the OTLP receiver.
	Endpoint string `json:"endpoint" jsonschema:"required,format=uri"`
	// Protocol: http/protobuf or grpc.
	Protocol string `json:"protocol,omitempty" jsonschema:"enum=http/protobuf,enum=grpc,default=http/protobuf"`
	// Headers added to every request; values are literals or secrets.
	Headers map[string]StringOrSecret `json:"headers,omitempty"`
	// ResourceAttributes added to the OTLP resource.
	ResourceAttributes map[string]string `json:"resourceAttributes,omitempty"`
	// Timeout of a send request.
	Timeout Duration `json:"timeout,omitempty" jsonschema:"default=10s"`
}

// BasicAuthSecrets are HTTP basic credentials read from secrets.
type BasicAuthSecrets struct {
	// Username secret.
	Username SecretRef `json:"username" jsonschema:"required"`
	// Password secret.
	Password SecretRef `json:"password" jsonschema:"required"`
}

// Queue is the bounded in-memory queue of a push output.
type Queue struct {
	// Capacity in samples; the oldest samples are dropped (with accounting)
	// when full.
	Capacity int `json:"capacity,omitempty" jsonschema:"minimum=1,default=50000"`
	// MaxBatch is the maximum number of samples per request.
	MaxBatch int `json:"maxBatch,omitempty" jsonschema:"minimum=1,default=5000"`
	// FlushInterval sends a partial batch after this delay.
	FlushInterval Duration `json:"flushInterval,omitempty" jsonschema:"default=5s"`
}
