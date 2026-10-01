// Package langfuse is the Langfuse plugin for woca's observability
// protocol: a thin composition of the generic OTLP exporter (sdk/otlp)
// with Langfuse's conventions — the ingestion endpoint and Basic auth, the
// real-time v4 ingestion header, and the attribute renames Langfuse's OTLP
// intake expects (session_id → langfuse.session.id, user_id →
// langfuse.user.id) plus its observation-type vocabulary. The span
// vocabulary itself (gen_ai.* attributes, run-derived trace grouping) is
// the standard protocol layer and lives in sdk/otlp.
//
// Delivery posture is the generic sink's: async, batched, lossy — Langfuse
// being unreachable drops exports without affecting the agent. Close
// flushes; hosts should defer it until shutdown.
package langfuse

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/projanvil/woca/sdk"
	"github.com/projanvil/woca/sdk/otlp"
)

// ingestionBase is Langfuse's OTLP intake root; the generic sink appends
// the standard /v1/traces path.
const ingestionBase = "/api/public/otel"

// Config configures the Langfuse sink.
type Config struct {
	// Host is the Langfuse base URL, e.g. https://cloud.langfuse.com. A
	// trailing slash is trimmed.
	Host string
	// PublicKey and SecretKey are the API keys Langfuse authenticates with
	// (Basic auth pk:sk).
	PublicKey string
	SecretKey string

	// Optional tuning, forwarded to the generic OTLP sink. Zero values
	// take its defaults (2s flush window, 512-slot queue, 50-span batches).
	FlushInterval time.Duration
	MaxQueue      int
	BatchSize     int
	HTTPClient    *http.Client
	// ServiceName labels the OTLP resource, default "woca-sdk".
	ServiceName string
}

// Sink exports harness spans to Langfuse. It implements sdk.TelemetrySink
// and is safe for concurrent use.
type Sink struct {
	*otlp.Sink
}

// New validates the config and starts the sink's background flusher.
func New(cfg Config) (*Sink, error) {
	host := strings.TrimSuffix(cfg.Host, "/")
	if host == "" {
		return nil, fmt.Errorf("langfuse: Host is required")
	}
	if cfg.PublicKey == "" || cfg.SecretKey == "" {
		return nil, fmt.Errorf("langfuse: PublicKey and SecretKey are required")
	}
	if cfg.ServiceName == "" {
		cfg.ServiceName = "woca-sdk" // the plugin's historical default
	}
	auth := "Basic " + base64.StdEncoding.EncodeToString([]byte(cfg.PublicKey+":"+cfg.SecretKey))
	s, err := otlp.New(otlp.Config{
		Endpoint: host + ingestionBase,
		Headers: map[string]string{
			"Authorization": auth,
			// Langfuse requires the v4 real-time ingestion header; without
			// it direct OTel ingestion is delayed by up to 10 minutes.
			"x-langfuse-ingestion-version": "4",
		},
		Decorate:      decorate,
		FlushInterval: cfg.FlushInterval,
		MaxQueue:      cfg.MaxQueue,
		BatchSize:     cfg.BatchSize,
		HTTPClient:    cfg.HTTPClient,
		ServiceName:   cfg.ServiceName,
	})
	if err != nil {
		return nil, err
	}
	return &Sink{Sink: s}, nil
}

// tagRenames maps woca's correlation tags onto Langfuse's OTel conventions
// — the same mapping the enterprise collector bridge performs
// (deploy/collector transform).
var tagRenames = map[string]string{
	"session_id": "langfuse.session.id",
	"user_id":    "langfuse.user.id",
}

// observationType maps a woca span kind onto Langfuse's observation
// vocabulary; Langfuse renders "generation" entries with model/usage
// dashboards.
func observationType(kind string) string {
	switch kind {
	case "llm":
		return "generation"
	case "tool":
		return "tool"
	default:
		return "span"
	}
}

// decorate applies Langfuse's conventions without mutating the input span.
func decorate(sp sdk.Span) sdk.Span {
	tags := make(map[string]string, len(sp.Tags)+1)
	for k, v := range sp.Tags {
		if r, ok := tagRenames[k]; ok {
			tags[r] = v
		} else {
			tags[k] = v
		}
	}
	tags["langfuse.observation.type"] = observationType(sp.Kind)
	sp.Tags = tags
	return sp
}
