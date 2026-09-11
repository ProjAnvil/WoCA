package mcp

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"
)

// badEndpointTransport builds a transport whose endpoint URL fails request
// construction (invalid percent-escape) without any network involvement.
func badEndpointTransport() *httpTransport {
	return &httpTransport{client: &http.Client{}, endpoint: "http://127.0.0.1/mcp%zz"}
}

// TestHTTPTransportInvalidEndpointURL covers the request-construction error
// branches of roundTrip, send, and listen.
func TestHTTPTransportInvalidEndpointURL(t *testing.T) {
	tr := badEndpointTransport()
	if _, err := tr.roundTrip(context.Background(), 1, []byte(`{"id":1}`)); err == nil {
		t.Fatal("roundTrip should fail on invalid endpoint URL")
	}
	if err := tr.send([]byte(`{"method":"x"}`)); err == nil {
		t.Fatal("send should fail on invalid endpoint URL")
	}
	if err := tr.listen(context.Background()); err == nil {
		t.Fatal("listen should fail on invalid endpoint URL")
	}
}

// TestHTTPTransportReadBodyError aborts the response mid-body so io.ReadAll
// fails after the 200 status was already received.
func TestHTTPTransportReadBodyError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":1,`))
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	}))
	defer ts.Close()
	tr := newHTTPTransport(ts.URL)
	if tr == nil {
		t.Fatal("nil transport")
	}
	if _, err := tr.roundTrip(context.Background(), 1, []byte(`{"id":1}`)); err == nil {
		t.Fatal("roundTrip should surface the truncated body")
	}
}

// TestHTTPTransportListenRefused targets a closed server: the GET fails with
// the context still live, taking the non-cancelled error branch.
func TestHTTPTransportListenRefused(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := ts.URL
	ts.Close()
	tr := newHTTPTransport(url)
	if tr == nil {
		t.Fatal("nil transport")
	}
	if err := tr.listen(context.Background()); err == nil || !strings.Contains(err.Error(), "listen") {
		t.Fatalf("listen on refused port = %v, want listen error", err)
	}
}

// TestHTTPTransportListenScannerTooLong sends a single SSE line larger than
// the scanner's token budget; the scanner error surfaces while ctx is alive.
func TestHTTPTransportListenScannerTooLong(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(bytes.Repeat([]byte("a"), 128*1024)) // no newline: one huge token
	}))
	defer ts.Close()
	tr := newHTTPTransport(ts.URL)
	if tr == nil {
		t.Fatal("nil transport")
	}
	if err := tr.listen(context.Background()); err == nil || !strings.Contains(err.Error(), "listen") {
		t.Fatalf("listen with oversized token = %v, want scanner error", err)
	}
}

// TestHTTPTransportListenCleanEnd: the stream ends (server closes) without a
// scanner error, so listen returns nil.
func TestHTTPTransportListenCleanEnd(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"jsonrpc\":\"2.0\",\"method\":\"notify\"}\n\n")
	}))
	defer ts.Close()
	tr := newHTTPTransport(ts.URL)
	if tr == nil {
		t.Fatal("nil transport")
	}
	if err := tr.listen(context.Background()); err != nil {
		t.Fatalf("clean listen end = %v, want nil", err)
	}
}

// TestParseSSEEdgeLines covers the skip branches: empty data, [DONE], and
// unparseable payloads before the real response.
func TestParseSSEEdgeLines(t *testing.T) {
	body := []byte("event: x\ndata:\ndata: [DONE]\ndata: not-json\ndata: {\"id\":7,\"result\":{\"ok\":true}}\n\n")
	resp, err := parseSSE(body, 7, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(resp.Result) != `{"ok":true}` {
		t.Fatalf("result = %s", resp.Result)
	}
}

// TestParseSSENoResponseForID: only notifications in the stream.
func TestParseSSENoResponseForID(t *testing.T) {
	body := []byte("data: {\"jsonrpc\":\"2.0\",\"method\":\"notify\",\"params\":{}}\n\n")
	if _, err := parseSSE(body, 3, nil); err == nil || !strings.Contains(err.Error(), "no response") {
		t.Fatalf("missing response error = %v", err)
	}
}

// TestRunOAuthRegistrationBadURL: a registration endpoint with a control
// character fails request construction before any dial.
func TestRunOAuthRegistrationBadURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Control character in the URL: http.NewRequest rejects it.
		fmt.Fprint(w, "{\"authorization_endpoint\":\"/authorize\",\"token_endpoint\":\"/token\",\"registration_endpoint\":\"http://127.0.0.1\\n/reg\"}")
	}))
	defer srv.Close()
	flow := OAuthFlow{ServerName: "badreg", Endpoint: srv.URL + "/mcp", Client: srv.Client()}
	if err := RunOAuth(context.Background(), flow); err == nil || !strings.Contains(err.Error(), "dynamic client registration") {
		t.Fatalf("bad registration URL = %v, want registration error", err)
	}
}

// TestStdioTransportReadError feeds a failing reader so roundTrip surfaces
// the read-response error.
func TestStdioTransportReadError(t *testing.T) {
	tr := newStdioTransportFromIO(io.Discard, iotest.ErrReader(io.ErrUnexpectedEOF))
	if _, err := tr.roundTrip(context.Background(), 1, []byte(`{"id":1}`)); err == nil {
		t.Fatal("roundTrip should surface reader failure")
	}
}

// TestClientEdgeBranches covers Close on a transport-less client, Listen on
// the stdio transport (no listen capability), and marshal failures.
func TestClientEdgeBranches(t *testing.T) {
	bare := &Client{name: "bare"}
	if err := bare.Close(); err != nil {
		t.Fatalf("Close with nil transport = %v", err)
	}

	c := NewClient("stdio", io.Discard, strings.NewReader(""), nil)
	if err := c.Listen(context.Background()); err != nil {
		t.Fatalf("stdio Listen = %v, want nil no-op", err)
	}
	if err := c.notify(rpcRequest{JSONRPC: "2.0", Method: "m", Params: make(chan int)}); err == nil {
		t.Fatal("notify with unmarshalable params should fail")
	}
	if _, err := c.request(context.Background(), "m", make(chan int)); err == nil {
		t.Fatal("request with unmarshalable params should fail")
	}
}

// TestAsToolInvalidPropertiesSchema: a non-object properties map fails
// coretools validation, so AsTool falls back to the empty object schema. Both
// the success and error closure of the fallback tool are exercised.
func TestAsToolInvalidPropertiesSchema(t *testing.T) {
	okLine := `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"hi"}]}}` + "\n"
	c := NewClient("x", &bytes.Buffer{}, io.MultiReader(strings.NewReader(okLine), iotest.ErrReader(io.ErrUnexpectedEOF)), nil)
	tool := c.AsTool("srv", Tool{
		Name:        "t",
		InputSchema: map[string]any{"type": "object", "properties": "not-an-object"},
	})
	if tool == nil {
		t.Fatal("fallback tool should be built")
	}
	if props, ok := tool.ArgsSchema()["properties"].(map[string]any); !ok || len(props) != 0 {
		t.Fatalf("fallback schema properties = %v, want empty object", tool.ArgsSchema()["properties"])
	}
	res, err := tool.Invoke(context.Background(), map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != "hi" {
		t.Fatalf("invoke content = %q", res.Content)
	}
	// The next read fails, so the fallback closure's error return runs too.
	if _, err := tool.Invoke(context.Background(), map[string]any{}); err == nil {
		t.Fatal("invoke after transport failure should error")
	}
}

// TestPromptContentTextNeitherObjectNorArray: a bare JSON number matches
// neither shape and yields the empty string.
func TestPromptContentTextNeitherObjectNorArray(t *testing.T) {
	if got := promptContentText([]byte("42")); got != "" {
		t.Fatalf("promptContentText(number) = %q, want empty", got)
	}
}
