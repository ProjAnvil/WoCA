package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/projanvil/langchain-golang/core/security"
)

// httpTransport implements transport over the MCP "streamable HTTP" transport:
// JSON-RPC requests are POSTed to an endpoint and the response may be a single
// JSON object or an SSE stream. The server's session id is threaded via the
// Mcp-Session-Id header.
type httpTransport struct {
	client   *http.Client
	endpoint string
	// token, when set, supplies a Bearer token for every request (MCP OAuth).
	token func() string

	// sessionMu guards session, which is updated concurrently by roundTrip.
	sessionMu sync.Mutex
	session   string

	notifier func(method string, params json.RawMessage)
}

func newHTTPTransport(endpoint string) *httpTransport {
	if err := security.ValidateURL(endpoint, mcpSSRFPolicy()); err != nil {
		return nil
	}
	return &httpTransport{
		client:   &http.Client{Timeout: 60 * time.Second},
		endpoint: endpoint,
	}
}

// NewStreamableHTTPAuth builds an MCP client whose HTTP requests carry a
// Bearer token from the supplier (MCP OAuth; empty tokens are skipped).
func NewStreamableHTTPAuth(name, endpoint string, token func() string) *Client {
	t := newHTTPTransport(endpoint)
	if t == nil {
		return nil
	}
	t.token = token
	return newClient(name, t)
}

// applyAuth sets the Authorization header when a token supplier is wired.
func (t *httpTransport) applyAuth(req *http.Request) {
	if t.token == nil {
		return
	}
	if tok := t.token(); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
}

// mcpSSRFPolicy allows loopback (a user explicitly configures a local MCP
// server) but still blocks private, link-local, and cloud-metadata targets.
// Loopback is whitelisted via AllowedHosts because the library's
// BlockPrivateIPs check also covers loopback.
func mcpSSRFPolicy() security.SSRFPolicy {
	p := security.DefaultSSRFPolicy()
	p.BlockLocalhost = false
	p.AllowedHosts = map[string]bool{
		"127.0.0.1": true,
		"localhost": true,
		"::1":       true,
	}
	return p
}

// NewStreamableHTTP builds an MCP client over the streamable HTTP transport.
// It returns nil when the endpoint fails SSRF validation (private, link-local,
// or cloud-metadata hosts; loopback is allowed).
func NewStreamableHTTP(name, endpoint string) *Client {
	t := newHTTPTransport(endpoint)
	if t == nil {
		return nil
	}
	return newClient(name, t)
}

func (t *httpTransport) setNotifier(fn func(string, json.RawMessage)) { t.notifier = fn }

// getSession returns the current session id, guarded for concurrent access.
func (t *httpTransport) getSession() string {
	t.sessionMu.Lock()
	defer t.sessionMu.Unlock()
	return t.session
}

// setSession records the server-provided session id, guarded for concurrent access.
func (t *httpTransport) setSession(s string) {
	t.sessionMu.Lock()
	t.session = s
	t.sessionMu.Unlock()
}

func (t *httpTransport) roundTrip(ctx context.Context, id int64, req []byte) (rpcResponse, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, t.endpoint, bytes.NewReader(req))
	if err != nil {
		return rpcResponse{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json, text/event-stream")
	if sid := t.getSession(); sid != "" {
		httpReq.Header.Set("Mcp-Session-Id", sid)
	}
	t.applyAuth(httpReq)

	resp, err := t.client.Do(httpReq)
	if err != nil {
		return rpcResponse{}, fmt.Errorf("mcp: streamable http: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		t.setSession(sid)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return rpcResponse{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return rpcResponse{}, fmt.Errorf("mcp: streamable http returned %d: %s", resp.StatusCode, body)
	}

	ct := resp.Header.Get("Content-Type")
	if strings.Contains(ct, "text/event-stream") {
		return parseSSE(body, id, t.notifier)
	}
	var out rpcResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return rpcResponse{}, fmt.Errorf("mcp: decode response: %w", err)
	}
	return out, nil
}

func (t *httpTransport) send(req []byte) error {
	httpReq, err := http.NewRequest(http.MethodPost, t.endpoint, bytes.NewReader(req))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json, text/event-stream")
	if sid := t.getSession(); sid != "" {
		httpReq.Header.Set("Mcp-Session-Id", sid)
	}
	t.applyAuth(httpReq)
	resp, err := t.client.Do(httpReq)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

func (t *httpTransport) close() error { return nil }

// listen opens a long-lived GET SSE connection and dispatches server
// notifications through the notifier until ctx is done or the response ends.
func (t *httpTransport) listen(ctx context.Context) error {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, t.endpoint, nil)
	if err != nil {
		return err
	}
	httpReq.Header.Set("Accept", "text/event-stream")
	if sid := t.getSession(); sid != "" {
		httpReq.Header.Set("Mcp-Session-Id", sid)
	}
	t.applyAuth(httpReq)
	// The listen connection is intentionally long-lived, so use a client
	// without a timeout; its lifetime is governed by ctx.
	c := *t.client
	c.Timeout = 0
	resp, err := c.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("mcp: streamable http listen: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var raw struct {
			ID     *int64          `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal([]byte(data), &raw); err != nil {
			continue
		}
		if raw.Method != "" && raw.ID == nil {
			if t.notifier != nil {
				t.notifier(raw.Method, raw.Params)
			}
		}
	}
	if err := sc.Err(); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("mcp: streamable http listen: %w", err)
	}
	return nil
}

// parseSSE extracts the JSON-RPC response matching id from an SSE body,
// dispatching notifications through notifier.
func parseSSE(body []byte, id int64, notifier func(string, json.RawMessage)) (rpcResponse, error) {
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var raw struct {
			ID     *int64          `json:"id"`
			Method string          `json:"method"`
			Result json.RawMessage `json:"result"`
			Error  *rpcError       `json:"error"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal([]byte(data), &raw); err != nil {
			continue
		}
		if raw.Method != "" && raw.ID == nil {
			if notifier != nil {
				notifier(raw.Method, raw.Params)
			}
			continue
		}
		if raw.ID != nil && *raw.ID == id {
			return rpcResponse{ID: id, Result: raw.Result, Error: raw.Error}, nil
		}
	}
	return rpcResponse{}, fmt.Errorf("mcp: no response for id %d in SSE stream", id)
}
