package langfuse

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/projanvil/woca/internal/tracing"
)

// captureServer records one OTLP export request.
type captureServer struct {
	t       *testing.T
	mu      sync.Mutex
	gotPath string
	gotAuth string
	gotVer  string
	body    []byte
	ready   chan struct{}
}

func newCaptureServer(t *testing.T) (*captureServer, *httptest.Server) {
	t.Helper()
	c := &captureServer{t: t, ready: make(chan struct{}, 8)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			c.t.Errorf("read body: %v", err)
		}
		c.mu.Lock()
		c.gotPath = r.URL.Path
		c.gotAuth = r.Header.Get("Authorization")
		c.gotVer = r.Header.Get("x-langfuse-ingestion-version")
		c.body = body
		c.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		c.ready <- struct{}{}
	}))
	t.Cleanup(srv.Close)
	return c, srv
}

func (c *captureServer) wait(t *testing.T) {
	t.Helper()
	select {
	case <-c.ready:
	case <-time.After(5 * time.Second):
		c.t.Fatal("no export request arrived")
	}
}

// spanAt is a convenience constructor for test spans.
func spanAt(name, kind, runID string, attrs map[string]any, tags map[string]string) tracing.Span {
	return tracing.Span{Name: name, Kind: kind, Start: time.Unix(1700000000, 0), DurationMS: 42,
		Attributes: attrs, Tags: tags}
}

func decodeSpans(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	var req struct {
		ResourceSpans []struct {
			Resource struct {
				Attributes []map[string]any `json:"attributes"`
			} `json:"resource"`
			ScopeSpans []struct {
				Scope struct {
					Name string `json:"name"`
				} `json:"scope"`
				Spans []map[string]any `json:"spans"`
			} `json:"scopeSpans"`
		} `json:"resourceSpans"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("decode OTLP body: %v\n%s", err, body)
	}
	if len(req.ResourceSpans) == 0 {
		t.Fatal("no resourceSpans")
	}
	res := req.ResourceSpans[0].Resource.Attributes
	var svc string
	for _, a := range res {
		if a["key"] == "service.name" {
			svc = a["value"].(map[string]any)["stringValue"].(string)
		}
	}
	if svc == "" {
		t.Fatal("resource missing service.name")
	}
	if len(req.ResourceSpans[0].ScopeSpans) != 1 {
		t.Fatalf("scopeSpans = %d, want 1", len(req.ResourceSpans[0].ScopeSpans))
	}
	return req.ResourceSpans[0].ScopeSpans[0].Spans
}

func attrOf(t *testing.T, sp map[string]any, key string) map[string]any {
	t.Helper()
	for _, a := range sp["attributes"].([]any) {
		m := a.(map[string]any)
		if m["key"] == key {
			return m["value"].(map[string]any)
		}
	}
	t.Fatalf("span %v has no attribute %q", sp["name"], key)
	return nil
}

func TestSinkExportsOTLPWithLangfuseConventions(t *testing.T) {
	cap, srv := newCaptureServer(t)

	s, err := New(Config{Host: srv.URL + "/", PublicKey: "pk-test", SecretKey: "sk-test",
		FlushInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	s.Record(ctx, spanAt("chat", "llm", "r1",
		map[string]any{"gen_ai.request.model": "claude-sonnet-4-5", "gen_ai.usage.input_tokens": 10},
		map[string]string{"run_id": "r1", "session_id": "sess1", "user_id": "u1"}))
	s.Record(ctx, spanAt("invoke_tool", "tool", "r1", nil, map[string]string{"run_id": "r1", "session_id": "sess1"}))
	s.Record(ctx, spanAt("chat", "llm", "r2", nil, map[string]string{"run_id": "r2"}))
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	cap.wait(t)

	if want := "/api/public/otel/v1/traces"; cap.gotPath != want {
		t.Fatalf("export path = %q, want %q", cap.gotPath, want)
	}
	if want := "Basic " + base64.StdEncoding.EncodeToString([]byte("pk-test:sk-test")); cap.gotAuth != want {
		t.Fatalf("Authorization = %q, want %q", cap.gotAuth, want)
	}
	if cap.gotVer != "4" {
		t.Fatalf("x-langfuse-ingestion-version = %q, want 4", cap.gotVer)
	}

	spans := decodeSpans(t, cap.body)
	if len(spans) != 3 {
		t.Fatalf("exported %d spans, want 3", len(spans))
	}
	if spans[0]["traceId"] != spans[1]["traceId"] {
		t.Fatal("spans of one run must share a traceId")
	}
	if spans[0]["traceId"] == spans[2]["traceId"] {
		t.Fatal("spans of different runs must not share a traceId")
	}
	if got := attrOf(t, spans[0], "langfuse.session.id")["stringValue"]; got != "sess1" {
		t.Fatalf("langfuse.session.id = %v", got)
	}
	if got := attrOf(t, spans[0], "langfuse.user.id")["stringValue"]; got != "u1" {
		t.Fatalf("langfuse.user.id = %v", got)
	}
	if got := attrOf(t, spans[0], "gen_ai.request.model")["stringValue"]; got != "claude-sonnet-4-5" {
		t.Fatalf("gen_ai.request.model = %v", got)
	}
	if got := attrOf(t, spans[0], "gen_ai.usage.input_tokens")["intValue"]; got != "10" {
		t.Fatalf("gen_ai.usage.input_tokens = %v, want protojson string \"10\"", got)
	}
	if got := attrOf(t, spans[0], "langfuse.observation.type")["stringValue"]; got != "generation" {
		t.Fatalf("llm observation type = %v", got)
	}
	if got := attrOf(t, spans[1], "langfuse.observation.type")["stringValue"]; got != "tool" {
		t.Fatalf("tool observation type = %v", got)
	}
	// Timing: nanosecond strings are present and ordered.
	if spans[0]["startTimeUnixNano"] == "" || spans[0]["endTimeUnixNano"] == "" {
		t.Fatalf("span missing unix-nano timestamps: %v", spans[0])
	}
}

func TestSinkMarksErrorStatus(t *testing.T) {
	cap, srv := newCaptureServer(t)
	s, err := New(Config{Host: srv.URL, PublicKey: "pk", SecretKey: "sk", FlushInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	s.Record(context.Background(), tracing.Span{Name: "chat", Kind: "llm", Status: "error",
		Error: "boom", Start: time.Unix(1700000000, 0), DurationMS: 1})
	s.Close()
	cap.wait(t)

	spans := decodeSpans(t, cap.body)
	st, ok := spans[0]["status"].(map[string]any)
	if !ok {
		t.Fatalf("span has no status: %v", spans[0])
	}
	// OTel STATUS_CODE_ERROR is 2.
	if st["code"] != float64(2) || st["message"] != "boom" {
		t.Fatalf("status = %v", st)
	}
}

func TestSinkDropsOverflowAndAccountingHolds(t *testing.T) {
	cap, srv := newCaptureServer(t)
	s, err := New(Config{Host: srv.URL, PublicKey: "pk", SecretKey: "sk",
		FlushInterval: time.Hour, MaxQueue: 2, BatchSize: 50})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		s.Record(context.Background(), spanAt(fmt.Sprintf("s%d", i), "llm", "r1", nil, nil))
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	cap.wait(t)
	sent, dropped := s.Stats()
	if dropped < 1 {
		t.Fatalf("expected at least one dropped span with MaxQueue=2, got sent=%d dropped=%d", sent, dropped)
	}
	if total := sent + dropped; total != 5 {
		t.Fatalf("sent+dropped = %d, want 5 (sent=%d dropped=%d)", total, sent, dropped)
	}
}

func TestSinkCountsFailedExportAsDropped(t *testing.T) {
	var reqs int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqs++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	s, err := New(Config{Host: srv.URL, PublicKey: "pk", SecretKey: "sk", FlushInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	s.Record(context.Background(), spanAt("chat", "llm", "r1", nil, nil))
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if reqs != 1 {
		t.Fatalf("export requests = %d, want 1", reqs)
	}
	sent, dropped := s.Stats()
	if sent != 0 || dropped != 1 {
		t.Fatalf("stats = sent %d / dropped %d, want 0 sent / 1 dropped", sent, dropped)
	}
}

func TestRecordAfterCloseIsNoop(t *testing.T) {
	cap, srv := newCaptureServer(t)
	s, err := New(Config{Host: srv.URL, PublicKey: "pk", SecretKey: "sk", FlushInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	s.Record(context.Background(), spanAt("chat", "llm", "r1", nil, nil))
	s.Close()
	cap.wait(t)
	// Post-close records are counted as dropped, never exported.
	s.Record(context.Background(), spanAt("late", "llm", "r1", nil, nil))
	if sent, dropped := s.Stats(); dropped != 1 {
		t.Fatalf("stats = sent %d / dropped %d, want 1 dropped", sent, dropped)
	}
	select {
	case <-cap.ready:
		t.Fatal("post-close record must not be exported")
	case <-time.After(200 * time.Millisecond):
	}
}

func TestNewValidatesConfig(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("empty config must fail")
	}
	if _, err := New(Config{Host: "https://x", PublicKey: "pk"}); err == nil {
		t.Fatal("missing secret key must fail")
	}
	if _, err := New(Config{Host: "https://x", PublicKey: "pk", SecretKey: "sk"}); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}
