package transport

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/projanvil/woca/native-client/internal/configstore"
)

type staticTarget struct {
	profile configstore.Profile
	ok      bool
	err     error
}

func (s staticTarget) ActiveProfile(context.Context) (configstore.Profile, bool, error) {
	return s.profile, s.ok, s.err
}

type staticCredentials struct {
	value Credentials
	err   error
}

func (s staticCredentials) Credentials(context.Context, string, *url.URL) (Credentials, error) {
	return s.value, s.err
}

func TestHandlerForwardsMethodPathQueryAndBody(t *testing.T) {
	type observedRequest struct {
		method      string
		path        string
		escapedPath string
		query       string
		body        string
	}
	observed := make(chan observedRequest, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
			return
		}
		observed <- observedRequest{r.Method, r.URL.Path, r.URL.EscapedPath(), r.URL.RawQuery, string(body)}
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"accepted":true}`)
	}))
	defer upstream.Close()

	handler := New(
		staticTarget{profile: configstore.Profile{ID: 7, BaseURL: upstream.URL}, ok: true},
		staticCredentials{},
		nil,
	)
	request := httptest.NewRequest(http.MethodPost, "http://native.local/api/runs/a%2Fb?q=1", strings.NewReader(`{"prompt":"hello"}`))
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	got := <-observed
	if got.method != http.MethodPost || got.path != "/api/runs/a/b" || got.escapedPath != "/api/runs/a%2Fb" || got.query != "q=1" || got.body != `{"prompt":"hello"}` {
		t.Fatalf("upstream request = %#v", got)
	}
	if response.Code != http.StatusCreated || response.Body.String() != `{"accepted":true}` {
		t.Fatalf("response = %d %q", response.Code, response.Body.String())
	}
}

func TestHandlerInjectsTrustedSessionAndDPoP(t *testing.T) {
	observed := make(chan http.Header, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observed <- r.Header.Clone()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	handler := New(
		staticTarget{profile: configstore.Profile{ID: 7, BaseURL: upstream.URL}, ok: true},
		staticCredentials{value: Credentials{Scheme: "DPoP", Session: "session-1", Proof: "proof-1"}},
		nil,
	)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://native.local/api/models", nil))

	headers := <-observed
	if got := headers.Get("Authorization"); got != "DPoP session-1" {
		t.Fatalf("Authorization = %q", got)
	}
	if got := headers.Get("DPoP"); got != "proof-1" {
		t.Fatalf("DPoP = %q", got)
	}
}

func TestHandlerStripsCallerAuthCookieAndForwardingHeaders(t *testing.T) {
	observed := make(chan http.Header, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observed <- r.Header.Clone()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	handler := New(
		staticTarget{profile: configstore.Profile{ID: 7, BaseURL: upstream.URL}, ok: true},
		staticCredentials{},
		nil,
	)
	request := httptest.NewRequest(http.MethodGet, "http://native.local/api/models", nil)
	stripped := []string{
		"Authorization", "DPoP", "Cookie", "Forwarded", "X-Forwarded-For", "X-Forwarded-Host",
		"X-Forwarded-Proto", "X-Forwarded-Port", "Connection", "Proxy-Connection", "Keep-Alive",
		"TE", "Trailer", "Transfer-Encoding", "Upgrade", "X-Evil-Hop",
	}
	for _, header := range stripped {
		request.Header.Set(header, "attacker")
	}
	request.Header.Set("Connection", "X-Evil-Hop")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	headers := <-observed
	for name, values := range headers {
		for _, value := range values {
			if strings.Contains(value, "attacker") {
				t.Fatalf("upstream received attacker-controlled %s: %q", name, value)
			}
		}
	}
}

func TestHandlerRejectsNonAPIPaths(t *testing.T) {
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		hits.Add(1)
	}))
	defer upstream.Close()
	handler := New(
		staticTarget{profile: configstore.Profile{BaseURL: upstream.URL}, ok: true},
		staticCredentials{},
		nil,
	)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://native.local/settings", nil))

	assertJSONError(t, response, http.StatusNotFound)
	if got := hits.Load(); got != 0 {
		t.Fatalf("upstream hits = %d", got)
	}
}

func TestHandlerReturnsServiceUnavailableWithoutActiveProfile(t *testing.T) {
	handler := New(
		staticTarget{ok: false},
		staticCredentials{value: Credentials{Scheme: "DPoP", Session: "session-secret", Proof: "proof-secret"}},
		nil,
	)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://native.local/api/models", nil))

	assertJSONError(t, response, http.StatusServiceUnavailable)
	if body := response.Body.String(); strings.Contains(body, "session-secret") || strings.Contains(body, "proof-secret") {
		t.Fatalf("response leaked credentials: %q", body)
	}
}

func TestHandlerRefusesCrossOriginRedirect(t *testing.T) {
	var redirectHits atomic.Int32
	redirectTarget := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		redirectHits.Add(1)
	}))
	defer redirectTarget.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", redirectTarget.URL+"/x")
		w.WriteHeader(http.StatusFound)
	}))
	defer upstream.Close()
	handler := New(
		staticTarget{profile: configstore.Profile{BaseURL: upstream.URL}, ok: true},
		staticCredentials{value: Credentials{Scheme: "DPoP", Session: "session-secret", Proof: "proof-secret"}},
		nil,
	)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://native.local/api/models", nil))

	assertJSONError(t, response, http.StatusBadGateway)
	if got := redirectHits.Load(); got != 0 {
		t.Fatalf("redirect target hits = %d", got)
	}
	if body := response.Body.String(); strings.Contains(body, redirectTarget.URL) || strings.Contains(body, "session-secret") || strings.Contains(body, "proof-secret") {
		t.Fatalf("response leaked redirect or credentials: %q", body)
	}
}

func TestHandlerPropagatesCancellation(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(cancelled)
	}))
	defer upstream.Close()
	handlerServer := httptest.NewServer(New(
		staticTarget{profile: configstore.Profile{BaseURL: upstream.URL}, ok: true},
		staticCredentials{},
		nil,
	))
	defer handlerServer.Close()

	ctx, cancel := context.WithCancel(context.Background())
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, handlerServer.URL+"/api/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		response, err := http.DefaultClient.Do(request)
		if response != nil {
			response.Body.Close()
		}
		result <- err
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream request did not start")
	}
	cancel()
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream context was not cancelled")
	}
	select {
	case <-result:
	case <-time.After(2 * time.Second):
		t.Fatal("client request did not finish after cancellation")
	}
}

func TestHandlerFlushesSSEIncrementally(t *testing.T) {
	testIncrementalFlush(t, "text/event-stream")
}

func TestHandlerFlushesUnclassifiedStreamsIncrementally(t *testing.T) {
	testIncrementalFlush(t, "application/octet-stream")
}

func testIncrementalFlush(t *testing.T, contentType string) {
	t.Helper()
	firstFrame := "event: first\ndata: {}\n\n"
	terminalFrame := "event: done\ndata: {}\n\n"
	releaseTerminal := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Content-Length", strconv.Itoa(len(firstFrame)+len(terminalFrame)))
		_, _ = io.WriteString(w, firstFrame)
		w.(http.Flusher).Flush()
		<-releaseTerminal
		_, _ = io.WriteString(w, terminalFrame)
	}))
	defer upstream.Close()
	handlerServer := httptest.NewServer(New(
		staticTarget{profile: configstore.Profile{BaseURL: upstream.URL}, ok: true},
		staticCredentials{},
		nil,
	))
	defer handlerServer.Close()

	type readResult struct {
		frame string
		err   error
	}
	result := make(chan readResult, 1)
	go func() {
		response, err := (&http.Client{Timeout: 2 * time.Second}).Get(handlerServer.URL + "/api/events")
		if err != nil {
			result <- readResult{err: err}
			return
		}
		defer response.Body.Close()
		frame := make([]byte, len(firstFrame))
		_, err = io.ReadFull(response.Body, frame)
		result <- readResult{frame: string(frame), err: err}
	}()

	select {
	case got := <-result:
		close(releaseTerminal)
		if got.err != nil {
			t.Fatal(got.err)
		}
		if got.frame != firstFrame {
			t.Fatalf("first frame = %q", got.frame)
		}
	case <-time.After(750 * time.Millisecond):
		close(releaseTerminal)
		t.Fatal("first stream frame was buffered until the terminal frame")
	}
}

func assertJSONError(t *testing.T, response *httptest.ResponseRecorder, wantStatus int) {
	t.Helper()
	if response.Code != wantStatus {
		t.Fatalf("status = %d, body = %q", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q", got)
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode JSON error: %v", err)
	}
	if body.Error == "" {
		t.Fatal("JSON error is empty")
	}
}
