package otlp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/projanvil/woca/sdk"
)

type capture struct {
	path    string
	headers http.Header
	body    []byte
	ready   chan struct{}
}

func newCapture(t *testing.T) (*capture, *httptest.Server) {
	t.Helper()
	c := &capture{headers: http.Header{}, ready: make(chan struct{}, 8)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		c.path = r.URL.Path
		c.headers = r.Header.Clone()
		c.body = body
		w.WriteHeader(http.StatusOK)
		c.ready <- struct{}{}
	}))
	t.Cleanup(srv.Close)
	return c, srv
}

func spansOf(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	var req struct {
		ResourceSpans []struct {
			Resource struct {
				Attributes []map[string]any `json:"attributes"`
			} `json:"resource"`
			ScopeSpans []struct {
				Spans []map[string]any `json:"spans"`
			} `json:"scopeSpans"`
		} `json:"resourceSpans"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("decode OTLP body: %v", err)
	}
	if len(req.ResourceSpans) != 1 || len(req.ResourceSpans[0].ScopeSpans) != 1 {
		t.Fatalf("unexpected resourceSpans shape: %s", body)
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

// TestSinkIsGenericOTLP: the standard traces path, custom headers passed
// through, and — unlike the Langfuse plugin — tags exported VERBATIM (no
// langfuse.* renames, no observation.type).
func TestSinkIsGenericOTLP(t *testing.T) {
	c, srv := newCapture(t)

	s, err := New(Config{
		Endpoint: srv.URL,
		Headers: map[string]string{
			"Authorization": "Bearer opentelemetry-token",
		},
		ResourceAttrs: map[string]string{"deployment.environment": "test"},
		FlushInterval: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	s.Record(context.Background(), sdk.Span{
		Name: "chat", Kind: "llm", Start: time.Unix(1700000000, 0), DurationMS: 5,
		Attributes: map[string]any{"gen_ai.request.model": "claude-sonnet-4-5"},
		Tags:       map[string]string{"run_id": "r1", "session_id": "sess1", "user_id": "u1"},
	})
	s.Record(context.Background(), sdk.Span{
		Name: "chat", Kind: "llm", Start: time.Unix(1700000001, 0), DurationMS: 5,
		Tags: map[string]string{"run_id": "r1"},
	})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	<-c.ready

	if want := "/v1/traces"; c.path != want {
		t.Fatalf("export path = %q, want %q", c.path, want)
	}
	if got := c.headers.Get("Authorization"); got != "Bearer opentelemetry-token" {
		t.Fatalf("Authorization = %q", got)
	}
	spans := spansOf(t, c.body)
	if len(spans) != 2 {
		t.Fatalf("spans = %d, want 2", len(spans))
	}
	// Same run → one trace; grouping is generic OTel behavior.
	if spans[0]["traceId"] != spans[1]["traceId"] {
		t.Fatal("spans of one run must share a traceId")
	}
	// Tags stay verbatim: no langfuse.* renaming, no observation.type.
	if got := attrOf(t, spans[0], "session_id")["stringValue"]; got != "sess1" {
		t.Fatalf("session_id = %v, want the verbatim tag", got)
	}
	for _, a := range spans[0]["attributes"].([]any) {
		if k := a.(map[string]any)["key"].(string); k == "langfuse.session.id" || k == "langfuse.observation.type" {
			t.Fatalf("generic sink leaked a Langfuse convention: %v", a)
		}
	}
	if got := attrOf(t, spans[0], "gen_ai.request.model")["stringValue"]; got != "claude-sonnet-4-5" {
		t.Fatalf("gen_ai.request.model = %v", got)
	}
	// Resource attributes from config plus service.name.
	var env, svc string
	var res map[string]any
	_ = json.Unmarshal(c.body, &struct {
		ResourceSpans []struct {
			Resource map[string]any `json:"resource"`
		} `json:"resourceSpans"`
	}{ResourceSpans: []struct {
		Resource map[string]any `json:"resource"`
	}{{Resource: res}}})
	var raw struct {
		ResourceSpans []struct {
			Resource struct {
				Attributes []map[string]any `json:"attributes"`
			} `json:"resource"`
		} `json:"resourceSpans"`
	}
	_ = json.Unmarshal(c.body, &raw)
	for _, a := range raw.ResourceSpans[0].Resource.Attributes {
		if a["key"] == "service.name" {
			svc = a["value"].(map[string]any)["stringValue"].(string)
		}
		if a["key"] == "deployment.environment" {
			env = a["value"].(map[string]any)["stringValue"].(string)
		}
	}
	if svc == "" || env != "test" {
		t.Fatalf("resource attrs: service.name=%q deployment.environment=%q", svc, env)
	}
}

// TestDecorate lets a backend plugin shape spans before export.
func TestDecorate(t *testing.T) {
	c, srv := newCapture(t)
	s, err := New(Config{
		Endpoint:      srv.URL,
		FlushInterval: time.Hour,
		Decorate: func(sp sdk.Span) sdk.Span {
			sp.Tags = map[string]string{"backend.tag": "added-by-plugin", "run_id": sp.Tags["run_id"]}
			return sp
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	s.Record(context.Background(), sdk.Span{Name: "chat", Kind: "llm", Start: time.Unix(1, 0), Tags: map[string]string{"run_id": "r1"}})
	s.Close()
	<-c.ready

	spans := spansOf(t, c.body)
	if got := attrOf(t, spans[0], "backend.tag")["stringValue"]; got != "added-by-plugin" {
		t.Fatalf("decorator attribute missing: %v", spans[0]["attributes"])
	}
	// run_id still drives trace grouping after decoration.
	if spans[0]["traceId"] == "" {
		t.Fatal("missing traceId")
	}
}

func TestNewValidates(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("empty endpoint must fail")
	}
}
