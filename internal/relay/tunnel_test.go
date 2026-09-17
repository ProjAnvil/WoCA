package relay

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// testRelay is the in-process full assembly (the edgegw_test.go pattern):
// a real Relay over httptest plus a created key.
type testRelay struct {
	ks     *KeyStore
	rl     *Relay
	srv    *httptest.Server
	key    Key
	rawKey string
}

func newTestRelay(t *testing.T) *testRelay {
	t.Helper()
	ks := openTestKeyStore(t)
	rl := NewRelay(ks, testSPA(), false, testLogger())
	srv := httptest.NewServer(rl.Handler())
	t.Cleanup(srv.Close)
	k, raw, err := ks.CreateKey(t.Context(), "home-mac")
	if err != nil {
		t.Fatal(err)
	}
	return &testRelay{ks: ks, rl: rl, srv: srv, key: k, rawKey: raw}
}

// login returns a client whose cookie jar carries the session.
func (tr *testRelay) login(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	c := &http.Client{Jar: jar}
	resp, err := c.Post(tr.srv.URL+"/api/auth/login", "application/json",
		strings.NewReader(`{"api_key":"`+tr.rawKey+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login: %d", resp.StatusCode)
	}
	return c
}

// sessionTokenOf logs in and extracts the cookie value for handcrafted
// requests.
func sessionTokenOf(t *testing.T, tr *testRelay) string {
	t.Helper()
	resp, err := http.Post(tr.srv.URL+"/api/auth/login", "application/json",
		strings.NewReader(`{"api_key":"`+tr.rawKey+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	for _, c := range resp.Cookies() {
		if c.Name == SessionCookieName {
			return c.Value
		}
	}
	t.Fatal("login issued no cookie")
	return ""
}

// claimGen parks a quick poll so the generation registers (the forward
// path's online check needs it).
func (tr *testRelay) claimGen(t *testing.T, gen string) {
	t.Helper()
	resp, _ := tr.pollOnce(t, gen, 10*time.Millisecond)
	_ = resp.Body.Close()
}

// pollOnce performs one bridge poll.
func (tr *testRelay) pollOnce(t *testing.T, gen string, wait time.Duration) (*http.Response, pollPayload) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet,
		fmt.Sprintf("%s/tunnel/poll?wait=%s", tr.srv.URL, wait), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+tr.rawKey)
	req.Header.Set(GenerationHeader, gen)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var p pollPayload
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	return resp, p
}

// respond uploads one bridge answer; body may stream.
func (tr *testRelay) respond(t *testing.T, gen, id string, status int, headers http.Header, body io.Reader) *http.Response {
	t.Helper()
	raw, _ := json.Marshal(map[string][]string(headers))
	req, err := http.NewRequest(http.MethodPost, tr.srv.URL+"/tunnel/respond/"+id, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+tr.rawKey)
	req.Header.Set(GenerationHeader, gen)
	req.Header.Set(StatusHeader, fmt.Sprint(status))
	req.Header.Set(HeadersHeader, base64.StdEncoding.EncodeToString(raw))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestTunnelRoundTrip(t *testing.T) {
	tr := newTestRelay(t)
	client := tr.login(t)
	tr.claimGen(t, "gen-1")

	go func() {
		resp, p := tr.pollOnce(t, "gen-1", 5*time.Second)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return
		}
		r := tr.respond(t, "gen-1", p.ID, http.StatusOK,
			http.Header{"Content-Type": {"application/json"}},
			strings.NewReader(`{"ok":true}`))
		_ = r.Body.Close()
	}()
	time.Sleep(100 * time.Millisecond)

	resp, err := client.Get(tr.srv.URL + "/api/sessions")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("tunneled GET: %d", resp.StatusCode)
	}
	data, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(data), `"ok":true`) {
		t.Fatalf("body: %s", data)
	}
}

func TestTunnelSSE(t *testing.T) {
	tr := newTestRelay(t)
	client := tr.login(t)
	tr.claimGen(t, "gen-1")

	go func() {
		resp, p := tr.pollOnce(t, "gen-1", 5*time.Second)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return
		}
		pr, pw := io.Pipe()
		go func() {
			_, _ = pw.Write([]byte("data: one\n\n"))
			time.Sleep(300 * time.Millisecond)
			_, _ = pw.Write([]byte("data: two\n\n"))
			_ = pw.Close()
		}()
		// The snapshot carries woca's SSE quirk (sse.go:30): Connection:
		// keep-alive must be filtered hop-by-hop (design §6.3).
		r := tr.respond(t, "gen-1", p.ID, http.StatusOK, http.Header{
			"Content-Type": {"text/event-stream"},
			"Connection":   {"keep-alive"},
		}, pr)
		_ = r.Body.Close()
	}()
	time.Sleep(100 * time.Millisecond)

	resp, err := client.Get(tr.srv.URL + "/api/runs/run-1/events")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content type %q (the flush-before-headers order must not lose it)", ct)
	}
	if got := resp.Header.Get("X-Accel-Buffering"); got != "no" {
		t.Fatalf("X-Accel-Buffering %q, want no", got)
	}
	if got := resp.Header.Get("Connection"); got != "" {
		t.Fatalf("hop-by-hop Connection leaked to the client: %q", got)
	}
	// Chunk immediacy: frame one arrives well before frame two is sent
	// (no aggregation, no buffering — design §6.3).
	start := time.Now()
	buf := make([]byte, 64)
	n, err := resp.Body.Read(buf)
	if err != nil || !strings.Contains(string(buf[:n]), "data: one") {
		t.Fatalf("first chunk: n=%d err=%v", n, err)
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("first frame took %v — the relay buffered instead of flushing", elapsed)
	}
	rest, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(rest), "data: two") {
		t.Fatalf("stream tail: %q", rest)
	}
}

func TestTunnelAuth(t *testing.T) {
	tr := newTestRelay(t)

	// No bearer at all.
	req, _ := http.NewRequest(http.MethodGet, tr.srv.URL+"/tunnel/poll", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous poll: %d, want 401", resp.StatusCode)
	}
	// Unknown key.
	req.Header.Set("Authorization", "Bearer wcr_"+strings.Repeat("A", 43))
	req.Header.Set(GenerationHeader, "gen-1")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unknown-key poll: %d, want 401", resp.StatusCode)
	}
	// Revoked key: 410 with the frontend-contract code.
	if _, ok, err := tr.ks.RevokeKey(t.Context(), "home-mac"); err != nil || !ok {
		t.Fatal("revoke setup failed")
	}
	req.Header.Set("Authorization", "Bearer "+tr.rawKey)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusGone || !strings.Contains(string(body), `"code":"revoked"`) {
		t.Fatalf("revoked poll: %d %s, want 410 + code revoked", resp.StatusCode, body)
	}
	// Client API without a session: 401 (the login gate).
	resp, err = http.Get(tr.srv.URL + "/api/sessions")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous API: %d, want 401", resp.StatusCode)
	}
	// The anonymous whitelist still answers.
	resp, err = http.Get(tr.srv.URL + "/api/health")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health: %d, want 200", resp.StatusCode)
	}
}

func TestGenerationConflictAndTakeover(t *testing.T) {
	tr := newTestRelay(t)
	now := time.Now()
	tr.rl.gens.now = func() time.Time { return now } // test clock

	tr.claimGen(t, "gen-a")
	// Competing live generation: 409 (design §6.2 / §9 matrix).
	resp, _ := tr.pollOnce(t, "gen-b", 10*time.Millisecond)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("competing generation: %d, want 409", resp.StatusCode)
	}
	// The lease expires; gen-b takes over.
	now = now.Add(GenerationStaleAfter + time.Second)
	tr.claimGen(t, "gen-b")
	// gen-a is a zombie: its respond 404s.
	r := tr.respond(t, "gen-a", "whatever", 200, nil, nil)
	_ = r.Body.Close()
	if r.StatusCode != http.StatusNotFound {
		t.Fatalf("zombie respond: %d, want 404", r.StatusCode)
	}
}

func TestRespondIsBridgeScoped(t *testing.T) {
	tr := newTestRelay(t)
	_, otherRaw, err := tr.ks.CreateKey(t.Context(), "other")
	if err != nil {
		t.Fatal(err)
	}
	client := tr.login(t)
	tr.claimGen(t, "gen-1")

	taken := make(chan pollPayload, 1)
	go func() {
		resp, p := tr.pollOnce(t, "gen-1", 5*time.Second)
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			taken <- p
		}
	}()
	time.Sleep(100 * time.Millisecond)
	go func() {
		resp, err := client.Get(tr.srv.URL + "/api/workspace")
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	var p pollPayload
	select {
	case p = <-taken:
	case <-time.After(3 * time.Second):
		t.Fatal("bridge never received the parked request")
	}
	// Cross-generation respond: 404 (design §6.1).
	r := tr.respond(t, "gen-2", p.ID, 200, nil, strings.NewReader("{}"))
	_ = r.Body.Close()
	if r.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-generation respond: %d, want 404", r.StatusCode)
	}
	// Cross-key respond: the other key's generation is not current → 404.
	req, _ := http.NewRequest(http.MethodPost, tr.srv.URL+"/tunnel/respond/"+p.ID,
		strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+otherRaw)
	req.Header.Set(GenerationHeader, "gen-9")
	req.Header.Set(StatusHeader, "200")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-key respond: %d, want 404", resp2.StatusCode)
	}
	// Unknown id on the right key+generation: 404.
	r = tr.respond(t, "gen-1", "made-up", 200, nil, strings.NewReader("{}"))
	_ = r.Body.Close()
	if r.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown-id respond: %d, want 404", r.StatusCode)
	}
	// A correctly-scoped respond still lands after the forgery attempts —
	// and unsticking the parked client keeps server shutdown from hanging.
	r = tr.respond(t, "gen-1", p.ID, 200, nil, strings.NewReader("{}"))
	_ = r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("properly-scoped respond: %d, want 200", r.StatusCode)
	}
}

func TestClientBodyLimit(t *testing.T) {
	tr := newTestRelay(t)
	client := tr.login(t)
	tr.claimGen(t, "gen-1")

	big := bytes.NewReader(make([]byte, MaxClientBody+1))
	resp, err := client.Post(tr.srv.URL+"/api/runs", "application/octet-stream", big)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("4MB+1 body: %d, want 413", resp.StatusCode)
	}
	if n := tr.rl.park.Len(tr.key.ID); n != 0 {
		t.Fatalf("oversized request was parked (n=%d)", n)
	}
}

func TestBridgeOffline(t *testing.T) {
	tr := newTestRelay(t)
	client := tr.login(t)
	resp, err := client.Get(tr.srv.URL + "/api/sessions")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable ||
		!strings.Contains(string(body), `"code":"bridge_offline"`) ||
		resp.Header.Get("Retry-After") != "5" {
		t.Fatalf("offline: %d %s retry=%q (want 503 bridge_offline + Retry-After 5)",
			resp.StatusCode, body, resp.Header.Get("Retry-After"))
	}
}

func TestCredentialsStrippedBeforeParking(t *testing.T) {
	tr := newTestRelay(t)
	tr.claimGen(t, "gen-1")

	taken := make(chan pollPayload, 1)
	go func() {
		resp, p := tr.pollOnce(t, "gen-1", 5*time.Second)
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			taken <- p
			r := tr.respond(t, "gen-1", p.ID, 200, nil, strings.NewReader("{}"))
			_ = r.Body.Close()
		}
	}()
	time.Sleep(100 * time.Millisecond)

	// Handcrafted request carrying BOTH credential channels (design §6.4:
	// the relay's own session credentials never cross the tunnel).
	req, _ := http.NewRequest(http.MethodGet, tr.srv.URL+"/api/sessions", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: sessionTokenOf(t, tr)})
	req.Header.Set("Authorization", "Bearer native-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("forward: %d", resp.StatusCode)
	}
	p := <-taken
	for name := range p.Headers {
		if strings.EqualFold(name, "Cookie") || strings.EqualFold(name, "Authorization") {
			t.Fatalf("credential header %q crossed the tunnel", name)
		}
	}
	if p.Client == "" {
		t.Fatal("client IP not propagated to the bridge")
	}
}

func TestPollMissingGenerationHeader(t *testing.T) {
	tr := newTestRelay(t)
	req, err := http.NewRequest(http.MethodGet, tr.srv.URL+"/tunnel/poll", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+tr.rawKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("poll without generation: %d, want 400", resp.StatusCode)
	}
}

func TestPollClientHangup(t *testing.T) {
	tr := newTestRelay(t)
	// The bridge abandons the poll mid-wait: the handler returns without
	// writing (the client observes a canceled request, not an answer).
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		tr.srv.URL+"/tunnel/poll?wait=5s", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+tr.rawKey)
	req.Header.Set(GenerationHeader, "gen-1")
	done := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
		}
		done <- err
	}()
	time.Sleep(150 * time.Millisecond) // let the poll park
	cancel()
	if err := <-done; err == nil {
		t.Fatal("canceled poll returned an answer")
	}
}

func TestRespondValidationFailures(t *testing.T) {
	tr := newTestRelay(t)
	tr.claimGen(t, "gen-1")

	respondRaw := func(gen, status, headersB64 string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, tr.srv.URL+"/tunnel/respond/req-x", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+tr.rawKey)
		if gen != "" {
			req.Header.Set(GenerationHeader, gen)
		}
		if status != "" {
			req.Header.Set(StatusHeader, status)
		}
		if headersB64 != "" {
			req.Header.Set(HeadersHeader, headersB64)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	// No generation header at all: not scoped to a live generation → 404.
	resp := respondRaw("", "200", "")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("generation-less respond: %d, want 404", resp.StatusCode)
	}
	// Unparseable / out-of-range status: 400.
	for _, status := range []string{"abc", "42", "1000"} {
		resp = respondRaw("gen-1", status, "")
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status %q: %d, want 400", status, resp.StatusCode)
		}
	}
	// Undecodable header snapshot: 400.
	resp = respondRaw("gen-1", "200", "!!!not-base64!!!")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad headers blob: %d, want 400", resp.StatusCode)
	}
	resp = respondRaw("gen-1", "200", base64.StdEncoding.EncodeToString([]byte("{not json")))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("non-JSON headers blob: %d, want 400", resp.StatusCode)
	}
	// A valid respond for an unknown id still 404s.
	resp = respondRaw("gen-1", "200", "")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown id: %d, want 404", resp.StatusCode)
	}
}

func TestRespondClientGone(t *testing.T) {
	tr := newTestRelay(t)
	client := tr.login(t)
	tr.claimGen(t, "gen-1")

	taken := make(chan pollPayload, 1)
	go func() {
		resp, p := tr.pollOnce(t, "gen-1", 5*time.Second)
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			taken <- p
		}
	}()
	time.Sleep(100 * time.Millisecond)

	// Park a client request, then hang it up before the bridge answers.
	clientCtx, cancelClient := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(clientCtx, http.MethodGet, tr.srv.URL+"/api/ghost", nil)
	if err != nil {
		t.Fatal(err)
	}
	answered := make(chan struct{})
	go func() {
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
		}
		close(answered)
	}()
	var p pollPayload
	select {
	case p = <-taken:
	case <-time.After(3 * time.Second):
		t.Fatal("bridge never received the parked request")
	}
	cancelClient()
	<-answered

	// The respond for the disconnected client: 410 (design §6.1 matrix).
	r := tr.respond(t, "gen-1", p.ID, 200, nil, strings.NewReader("{}"))
	_ = r.Body.Close()
	if r.StatusCode != http.StatusGone {
		t.Fatalf("respond for gone client: %d, want 410", r.StatusCode)
	}
}

func TestDecodeHeaders(t *testing.T) {
	// Empty snapshot: empty header set, no error.
	h, err := decodeHeaders("")
	if err != nil || len(h) != 0 {
		t.Fatalf("empty snapshot: %v %v", h, err)
	}
	if _, err := decodeHeaders("!!!not-base64!!!"); err == nil {
		t.Fatal("bad base64 must fail")
	}
	if _, err := decodeHeaders(base64.StdEncoding.EncodeToString([]byte("{nope"))); err == nil {
		t.Fatal("bad JSON must fail")
	}
	// Hop-by-hop headers are stripped; the Connection token's nominees too.
	raw, _ := json.Marshal(map[string][]string{
		"Content-Type": {"application/json"},
		"Connection":   {"keep-alive, X-Trace"},
		"Keep-Alive":   {"timeout=5"},
		"X-Trace":      {"forged"},
	})
	h, err = decodeHeaders(base64.StdEncoding.EncodeToString(raw))
	if err != nil {
		t.Fatal(err)
	}
	if got := h.Get("Content-Type"); got != "application/json" {
		t.Fatalf("ordinary header lost: %q", got)
	}
	for _, gone := range []string{"Connection", "Keep-Alive", "X-Trace"} {
		if h.Get(gone) != "" {
			t.Fatalf("hop-by-hop header %s survived", gone)
		}
	}
}
