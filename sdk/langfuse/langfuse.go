// Package langfuse exports woca harness spans directly to a Langfuse
// instance over OTLP/HTTP, giving the embedded (sdk) form — and any form
// without the enterprise collector bridge — native Langfuse reporting.
//
// The sink implements sdk.TelemetrySink: pass it to sdk.WithTelemetry and
// every span the harness records (LLM calls, tool invocations, skill loads)
// is exported with the run/session identity Langfuse groups by. The
// attribute keys already follow the OTel GenAI semantic conventions
// (gen_ai.*), so no translation is needed; woca's correlation tags are
// renamed onto Langfuse's conventions exactly the way the enterprise
// collector bridge does it (deploy/collector transform/langfuse).
//
// Delivery posture mirrors the collector bridge: async, batched, and lossy
// by design — when Langfuse is unreachable, exports fail fast and drop
// (bounded queue, no retry), never affecting the agent. Close flushes the
// pending batch; hosts should defer Close until shutdown.
package langfuse

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/projanvil/woca/sdk"
)

// Default delivery tuning.
const (
	defaultFlushInterval = 2 * time.Second
	defaultMaxQueue      = 512
	defaultBatchSize     = 50
	defaultTimeout       = 10 * time.Second
	ingestionPath        = "/api/public/otel/v1/traces"
	// Langfuse requires the v4 real-time ingestion header; without it
	// direct OTel ingestion is delayed by up to 10 minutes.
	ingestionVersion = "4"
)

// Config configures the Sink.
type Config struct {
	// Host is the Langfuse base URL, e.g. https://cloud.langfuse.com. A
	// trailing slash is trimmed.
	Host string
	// PublicKey and SecretKey are the API keys Langfuse authenticates with
	// (Basic auth pk:sk).
	PublicKey string
	SecretKey string

	// Optional tuning. Zero values take the defaults shown in the fields
	// below.
	FlushInterval time.Duration // batch window, default 2s
	MaxQueue      int           // queued spans before dropping, default 512
	BatchSize     int           // early-flush threshold, default 50
	HTTPClient    *http.Client  // default: 10s timeout
	// ServiceName labels the OTLP resource, default "woca-sdk".
	ServiceName string
}

// Sink is a batching OTLP/HTTP exporter for Langfuse. It implements
// sdk.TelemetrySink and is safe for concurrent use.
type Sink struct {
	cfg       Config
	auth      string
	client    *http.Client
	tracesURL string

	mu      sync.Mutex
	pending []sdk.Span
	closed  bool

	sent    atomic.Uint64
	dropped atomic.Uint64

	wake    chan struct{}
	done    chan struct{}
	flushed chan struct{}
	once    sync.Once
}

// New validates the config and starts the sink's background flusher.
func New(cfg Config) (*Sink, error) {
	cfg.Host = strings.TrimSuffix(cfg.Host, "/")
	if cfg.Host == "" {
		return nil, fmt.Errorf("langfuse: Host is required")
	}
	if cfg.PublicKey == "" || cfg.SecretKey == "" {
		return nil, fmt.Errorf("langfuse: PublicKey and SecretKey are required")
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = defaultFlushInterval
	}
	if cfg.MaxQueue <= 0 {
		cfg.MaxQueue = defaultMaxQueue
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = defaultBatchSize
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: defaultTimeout}
	}
	if cfg.ServiceName == "" {
		cfg.ServiceName = "woca-sdk"
	}
	s := &Sink{
		cfg:       cfg,
		auth:      "Basic " + base64.StdEncoding.EncodeToString([]byte(cfg.PublicKey+":"+cfg.SecretKey)),
		client:    cfg.HTTPClient,
		tracesURL: cfg.Host + ingestionPath,
		wake:      make(chan struct{}, 1),
		done:      make(chan struct{}),
		flushed:   make(chan struct{}),
	}
	go s.loop()
	return s, nil
}

// Stats reports delivery accounting: spans accepted by a 2xx export
// (Sent) and spans discarded (queue overflow, export failure, or arrival
// after Close). Sent+Dropped equals the number of Record calls.
func (s *Sink) Stats() (sent, dropped uint64) {
	return s.sent.Load(), s.dropped.Load()
}

// Record queues one span for export. It never blocks and never fails the
// caller: overflow and closed-sink arrivals are counted as dropped.
func (s *Sink) Record(_ context.Context, sp sdk.Span) {
	s.mu.Lock()
	if s.closed || len(s.pending) >= s.cfg.MaxQueue {
		s.mu.Unlock()
		s.dropped.Add(1)
		return
	}
	s.pending = append(s.pending, sp)
	batchFull := len(s.pending) >= s.cfg.BatchSize
	s.mu.Unlock()
	if batchFull {
		select {
		case s.wake <- struct{}{}:
		default:
		}
	}
}

// Close stops the flusher after one final flush. Safe to call more than
// once; later calls return nil.
func (s *Sink) Close() error {
	s.once.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		close(s.done)
		<-s.flushed
	})
	return nil
}

func (s *Sink) loop() {
	defer close(s.flushed)
	ticker := time.NewTicker(s.cfg.FlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.done:
			s.flush()
			return
		case <-ticker.C:
			s.flush()
		case <-s.wake:
			s.flush()
		}
	}
}

// flush exports the queued batch. Failures drop the batch (lossy by
// design) and count it as dropped.
func (s *Sink) flush() {
	s.mu.Lock()
	batch := s.pending
	s.pending = nil
	s.mu.Unlock()
	if len(batch) == 0 {
		return
	}
	if err := s.export(batch); err != nil {
		s.dropped.Add(uint64(len(batch)))
		return
	}
	s.sent.Add(uint64(len(batch)))
}

func (s *Sink) export(spans []sdk.Span) error {
	body, err := json.Marshal(exportRequest{ResourceSpans: []resourceSpans{{
		Resource: resource{Attributes: []otlpAttr{
			strAttr("service.name", s.cfg.ServiceName),
		}},
		ScopeSpans: []scopeSpans{{
			Scope: otlpScope{Name: "woca", Version: "1"},
			Spans: convertSpans(spans),
		}},
	}}})
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, s.tracesURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", s.auth)
	req.Header.Set("x-langfuse-ingestion-version", ingestionVersion)
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("langfuse: export status %d", resp.StatusCode)
	}
	return nil
}

// OTLP/JSON export payload (opentelemetry-proto field names, protojson
// encoding: 64-bit ints serialize as strings).

type exportRequest struct {
	ResourceSpans []resourceSpans `json:"resourceSpans"`
}

type resourceSpans struct {
	Resource   resource     `json:"resource"`
	ScopeSpans []scopeSpans `json:"scopeSpans"`
}

type resource struct {
	Attributes []otlpAttr `json:"attributes"`
}

type scopeSpans struct {
	Scope otlpScope  `json:"scope"`
	Spans []otlpSpan `json:"spans"`
}

type otlpScope struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type otlpSpan struct {
	TraceID           string      `json:"traceId"`
	SpanID            string      `json:"spanId"`
	Name              string      `json:"name"`
	Kind              int         `json:"kind"` // 1 = INTERNAL
	StartTimeUnixNano string      `json:"startTimeUnixNano"`
	EndTimeUnixNano   string      `json:"endTimeUnixNano"`
	Attributes        []otlpAttr  `json:"attributes"`
	Status            *otlpStatus `json:"status,omitempty"`
}

type otlpStatus struct {
	Code    int    `json:"code"` // 2 = ERROR
	Message string `json:"message,omitempty"`
}

type otlpAttr struct {
	Key   string        `json:"key"`
	Value otlpAttrValue `json:"value"`
}

type otlpAttrValue struct {
	StringValue *string  `json:"stringValue,omitempty"`
	BoolValue   *bool    `json:"boolValue,omitempty"`
	IntValue    *string  `json:"intValue,omitempty"`
	DoubleValue *float64 `json:"doubleValue,omitempty"`
}

func strAttr(k, v string) otlpAttr { return otlpAttr{Key: k, Value: otlpAttrValue{StringValue: &v}} }

// Tag renames onto Langfuse's OTel conventions — the same mapping the
// enterprise collector bridge performs (deploy/collector transform).
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

func convertSpans(spans []sdk.Span) []otlpSpan {
	out := make([]otlpSpan, 0, len(spans))
	for _, sp := range spans {
		attrs := make([]otlpAttr, 0, len(sp.Tags)+len(sp.Attributes)+2)
		var runID string
		for k, v := range sp.Tags {
			if k == "run_id" {
				runID = v
			}
			if rename, ok := tagRenames[k]; ok {
				k = rename
			}
			attrs = append(attrs, strAttr(k, v))
		}
		attrs = append(attrs, strAttr("langfuse.observation.type", observationType(sp.Kind)))
		for k, v := range sp.Attributes {
			attrs = append(attrs, typedAttr(k, v))
		}

		startNano := sp.Start.UnixNano()
		endNano := sp.Start.Add(time.Duration(sp.DurationMS) * time.Millisecond).UnixNano()
		os := otlpSpan{
			TraceID:           traceIDForRun(runID),
			SpanID:            randID(8),
			Name:              sp.Name,
			Kind:              1,
			StartTimeUnixNano: fmt.Sprintf("%d", startNano),
			EndTimeUnixNano:   fmt.Sprintf("%d", endNano),
			Attributes:        attrs,
		}
		if sp.Status == "error" || sp.Error != "" {
			os.Status = &otlpStatus{Code: 2, Message: sp.Error}
		}
		out = append(out, os)
	}
	return out
}

// traceIDForRun derives a stable 16-byte trace id from the run id, so every
// span of one run lands in one Langfuse trace; spans without run identity
// get a fresh trace each.
func traceIDForRun(runID string) string {
	if runID == "" {
		return randID(16)
	}
	sum := sha256.Sum256([]byte("woca-run:" + runID))
	return hex.EncodeToString(sum[:16])
}

func randID(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand does not fail on supported platforms; a constant
		// fallback keeps reporting alive instead of panicking.
		return strings.Repeat("0", n*2)
	}
	return hex.EncodeToString(b)
}

// typedAttr maps a telemetry attribute value onto protojson's typed value
// union (int64 as string).
func typedAttr(k string, v any) otlpAttr {
	val := otlpAttrValue{}
	switch t := v.(type) {
	case string:
		val.StringValue = &t
	case bool:
		val.BoolValue = &t
	case int:
		s := fmt.Sprintf("%d", t)
		val.IntValue = &s
	case int32:
		s := fmt.Sprintf("%d", t)
		val.IntValue = &s
	case int64:
		s := fmt.Sprintf("%d", t)
		val.IntValue = &s
	case float32:
		f := float64(t)
		val.DoubleValue = &f
	case float64:
		val.DoubleValue = &t
	default:
		s := fmt.Sprintf("%v", t)
		val.StringValue = &s
	}
	return otlpAttr{Key: k, Value: val}
}
