package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/projanvil/woca/native-client/internal/configstore"
	wailsassetserver "github.com/wailsapp/wails/v2/pkg/assetserver"
	wailsassetoptions "github.com/wailsapp/wails/v2/pkg/options/assetserver"
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

type countingTarget struct {
	calls   *atomic.Int32
	profile configstore.Profile
	ok      bool
}

func (s countingTarget) ActiveProfile(context.Context) (configstore.Profile, bool, error) {
	s.calls.Add(1)
	return s.profile, s.ok, nil
}

type countingCredentials struct {
	calls *atomic.Int32
}

func (s countingCredentials) Credentials(context.Context, string, *url.URL) (Credentials, error) {
	s.calls.Add(1)
	return Credentials{}, nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type testRuntimeAssets struct{}

func (testRuntimeAssets) DesktopIPC() []byte       { return nil }
func (testRuntimeAssets) WebsocketIPC() []byte     { return nil }
func (testRuntimeAssets) RuntimeDesktopJS() []byte { return []byte("// test runtime") }

type firstWriteHeaderWins struct {
	header http.Header
	status int
	body   bytes.Buffer
}

type trackingBody struct {
	reader *strings.Reader
	reads  atomic.Int32
	closed atomic.Bool
}

func newTrackingBody(value string) *trackingBody {
	return &trackingBody{reader: strings.NewReader(value)}
}

func (b *trackingBody) Read(buffer []byte) (int, error) {
	b.reads.Add(1)
	return b.reader.Read(buffer)
}

func (b *trackingBody) Close() error {
	b.closed.Store(true)
	return nil
}

func newFirstWriteHeaderWins() *firstWriteHeaderWins {
	return &firstWriteHeaderWins{header: make(http.Header)}
}

func (w *firstWriteHeaderWins) Header() http.Header {
	return w.header
}

func (w *firstWriteHeaderWins) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func (w *firstWriteHeaderWins) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.body.Write(body)
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
	request.Header.Set("TE", "trailers")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	headers := <-observed
	for _, name := range stripped {
		if values, ok := headers[http.CanonicalHeaderKey(name)]; ok {
			t.Errorf("upstream received forbidden %s header: %q", name, values)
		}
	}
}

func TestHandlerStripsNativeRequestIDBeforeUpstream(t *testing.T) {
	observed := make(chan http.Header, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observed <- r.Header.Clone()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	registry := NewCancellationRegistry()
	handler := NewWithCancellation(
		staticTarget{profile: configstore.Profile{BaseURL: upstream.URL}, ok: true},
		staticCredentials{},
		nil,
		registry,
	)
	request := httptest.NewRequest(http.MethodGet, "http://native.local/api/models", nil)
	request.Header.Set(RequestIDHeader, requestID("H"))

	handler.ServeHTTP(httptest.NewRecorder(), request)

	if values := (<-observed).Values(RequestIDHeader); len(values) != 0 {
		t.Fatalf("upstream received native request ID: %q", values)
	}
}

func TestHandlerRejectsInvalidAndDuplicateNativeRequestIDsBeforeLookup(t *testing.T) {
	for _, configure := range []func(*http.Request){
		func(request *http.Request) { request.Header.Set(RequestIDHeader, "invalid") },
		func(request *http.Request) {
			request.Header.Add(RequestIDHeader, requestID("I"))
			request.Header.Add(RequestIDHeader, requestID("J"))
		},
	} {
		registry := NewCancellationRegistry()
		var targetCalls atomic.Int32
		var credentialCalls atomic.Int32
		var roundTrips atomic.Int32
		handler := NewWithCancellation(
			countingTarget{calls: &targetCalls, profile: configstore.Profile{BaseURL: "http://localhost"}, ok: true},
			countingCredentials{calls: &credentialCalls},
			&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				roundTrips.Add(1)
				return nil, nil
			})},
			registry,
		)
		request := httptest.NewRequest(http.MethodGet, "http://native.local/api/models", nil)
		configure(request)
		response := httptest.NewRecorder()

		handler.ServeHTTP(response, request)

		if response.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, body = %q", response.Code, response.Body.String())
		}
		if targetCalls.Load() != 0 || credentialCalls.Load() != 0 || roundTrips.Load() != 0 {
			t.Fatalf("calls: target=%d credentials=%d upstream=%d", targetCalls.Load(), credentialCalls.Load(), roundTrips.Load())
		}
	}

	registry := NewCancellationRegistry()
	id := requestID("K")
	_, unregister, ok := registry.register(t.Context(), id)
	if !ok {
		t.Fatal("precondition registration failed")
	}
	defer unregister()
	var targetCalls atomic.Int32
	handler := NewWithCancellation(
		countingTarget{calls: &targetCalls, profile: configstore.Profile{BaseURL: "http://localhost"}, ok: true},
		staticCredentials{}, nil, registry,
	)
	request := httptest.NewRequest(http.MethodGet, "http://native.local/api/models", nil)
	request.Header.Set(RequestIDHeader, id)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || targetCalls.Load() != 0 {
		t.Fatalf("duplicate response = %d %q, target calls = %d", response.Code, response.Body.String(), targetCalls.Load())
	}
}

func TestHandlerRejectsAllNativeRequestIDsDuringCancellationOverflowBeforeLookup(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)
	now := start
	registry := newCancellationRegistryWithClock(func() time.Time { return now })
	for index := range maxPendingCancellations {
		if !registry.Cancel(indexedRequestID(index)) {
			t.Fatalf("Cancel() rejected pending ID %d", index)
		}
	}
	if !registry.Cancel(requestID("J")) {
		t.Fatal("Cancel() did not enter fail-closed overflow state")
	}
	now = start.Add(pendingCancellationTTL - time.Nanosecond)
	if !registry.Cancel(requestID("K")) {
		t.Fatal("Cancel() rejected the last valid overflow cancellation")
	}
	now = start.Add(pendingCancellationTTL + time.Nanosecond)
	var targetCalls atomic.Int32
	var credentialCalls atomic.Int32
	var roundTrips atomic.Int32
	handler := NewWithCancellation(
		countingTarget{calls: &targetCalls, profile: configstore.Profile{BaseURL: "http://localhost"}, ok: true},
		countingCredentials{calls: &credentialCalls},
		&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			roundTrips.Add(1)
			return nil, nil
		})},
		registry,
	)
	for _, id := range []string{indexedRequestID(0), requestID("J"), requestID("K"), requestID("Q")} {
		request := httptest.NewRequest(http.MethodGet, "http://native.local/api/models", nil)
		request.Header.Set(RequestIDHeader, id)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("request %q status = %d, body = %q", id, response.Code, response.Body.String())
		}
	}
	if targetCalls.Load() != 0 || credentialCalls.Load() != 0 || roundTrips.Load() != 0 {
		t.Fatalf("calls during overflow: target=%d credentials=%d upstream=%d", targetCalls.Load(), credentialCalls.Load(), roundTrips.Load())
	}
}

func TestHandlerRejectsCancelBeforeRegisterWithoutLookup(t *testing.T) {
	registry := NewCancellationRegistry()
	id := requestID("L")
	if !registry.Cancel(id) {
		t.Fatal("Cancel() rejected a valid pre-registration cancellation")
	}
	var targetCalls atomic.Int32
	var credentialCalls atomic.Int32
	var roundTrips atomic.Int32
	handler := NewWithCancellation(
		countingTarget{calls: &targetCalls, profile: configstore.Profile{BaseURL: "http://localhost"}, ok: true},
		countingCredentials{calls: &credentialCalls},
		&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			roundTrips.Add(1)
			return nil, nil
		})},
		registry,
	)
	request := httptest.NewRequest(http.MethodGet, "http://native.local/api/events", nil)
	request.Header.Set(RequestIDHeader, id)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %q", response.Code, response.Body.String())
	}
	if targetCalls.Load() != 0 || credentialCalls.Load() != 0 || roundTrips.Load() != 0 {
		t.Fatalf("calls after pre-cancel: target=%d credentials=%d upstream=%d", targetCalls.Load(), credentialCalls.Load(), roundTrips.Load())
	}
}

func TestHandlerNativeCancellationBridgesBackgroundContext(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(cancelled)
	}))
	defer upstream.Close()
	registry := NewCancellationRegistry()
	handler := NewWithCancellation(
		staticTarget{profile: configstore.Profile{BaseURL: upstream.URL}, ok: true},
		staticCredentials{}, nil, registry,
	)
	id := requestID("L")
	request := httptest.NewRequest(http.MethodGet, "http://native.local/api/events", nil)
	request.Header.Set(RequestIDHeader, id)
	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(httptest.NewRecorder(), request)
		close(done)
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream request did not start")
	}
	if !registry.Cancel(id) {
		t.Fatal("Cancel() did not find the Wails-like request")
	}
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("explicit native cancellation did not release upstream")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return after explicit native cancellation")
	}
}

func TestHandlerNaturallyUnregistersNativeRequestID(t *testing.T) {
	var roundTrips atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		roundTrips.Add(1)
		return &http.Response{
			StatusCode: http.StatusNoContent,
			Header:     make(http.Header),
			Body:       http.NoBody,
			Request:    request,
		}, nil
	})}
	registry := NewCancellationRegistry()
	handler := NewWithCancellation(
		staticTarget{profile: configstore.Profile{BaseURL: "http://localhost"}, ok: true},
		staticCredentials{}, client, registry,
	)
	id := requestID("M")
	for range 2 {
		request := httptest.NewRequest(http.MethodGet, "http://native.local/api/models", nil)
		request.Header.Set(RequestIDHeader, id)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusNoContent {
			t.Fatalf("response = %d %q", response.Code, response.Body.String())
		}
	}
	if roundTrips.Load() != 2 {
		t.Fatalf("upstream round trips = %d, want 2", roundTrips.Load())
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

func TestWailsAssetServerRejectsInjectionMatchedAPIPathsBeforeLookup(t *testing.T) {
	tests := []struct {
		name        string
		requestURL  string
		status      int
		contentType string
		body        string
	}{
		{
			name:        "API root trailing slash is an exact injection match",
			requestURL:  "http://native.local/api/",
			status:      http.StatusNoContent,
			contentType: "application/json",
		},
		{
			name:        "API root index document is an exact injection match",
			requestURL:  "http://native.local/api/index.html",
			status:      http.StatusNoContent,
			contentType: "application/json",
		},
		{
			name:        "trailing slash would replace upstream 404 with default HTML",
			requestURL:  "http://native.local/api/models/",
			status:      http.StatusNotFound,
			contentType: "application/json",
			body:        `{"upstream":"missing"}`,
		},
		{
			name:        "index document would receive runtime injection",
			requestURL:  "http://native.local/api/models/index.html",
			status:      http.StatusOK,
			contentType: "text/html",
			body:        "<html><head></head><body>upstream</body></html>",
		},
		{
			name:        "escaped trailing slash is matched after URL decoding",
			requestURL:  "http://native.local/api/models%2F",
			status:      http.StatusNoContent,
			contentType: "application/json",
		},
		{
			name:        "escaped index document is matched after URL decoding",
			requestURL:  "http://native.local/api/models/%69ndex%2Ehtml",
			status:      http.StatusNoContent,
			contentType: "application/json",
		},
		{
			name:        "traversal ending in index document must not fall back to HTML",
			requestURL:  "http://native.local/api/models/%2e%2e/index.html",
			status:      http.StatusNoContent,
			contentType: "application/json",
		},
		{
			name:        "canonical root index alias must not serve the embedded application",
			requestURL:  "http://native.local/api/%2e%2e/index.html",
			status:      http.StatusNoContent,
			contentType: "application/json",
		},
		{
			name:        "duplicate slash still triggers suffix matching",
			requestURL:  "http://native.local/api/models//",
			status:      http.StatusNoContent,
			contentType: "application/json",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var targetCalls atomic.Int32
			var credentialCalls atomic.Int32
			var roundTrips atomic.Int32
			client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				roundTrips.Add(1)
				return &http.Response{
					StatusCode: tt.status,
					Header:     http.Header{"Content-Type": []string{tt.contentType}},
					Body:       io.NopCloser(strings.NewReader(tt.body)),
					Request:    request,
				}, nil
			})}
			handler := New(
				countingTarget{calls: &targetCalls, profile: configstore.Profile{BaseURL: "https://enterprise.example.com"}, ok: true},
				countingCredentials{calls: &credentialCalls},
				client,
			)
			server, err := wailsassetserver.NewAssetServer(
				"",
				wailsassetoptions.Options{
					Assets:     fstest.MapFS{"index.html": {Data: []byte("<html><head></head><body>app</body></html>")}},
					Handler:    handler,
					Middleware: AssetDocumentGuard,
				},
				false,
				nil,
				testRuntimeAssets{},
			)
			if err != nil {
				t.Fatalf("NewAssetServer() error = %v", err)
			}

			response := httptest.NewRecorder()
			server.ServeHTTP(response, httptest.NewRequest(http.MethodGet, tt.requestURL, nil))

			assertJSONError(t, response, http.StatusBadRequest)
			if got := response.Body.String(); got != `{"error":"unsupported API path"}` {
				t.Fatalf("body = %q", got)
			}
			if targetCalls.Load() != 0 || credentialCalls.Load() != 0 || roundTrips.Load() != 0 {
				t.Fatalf("calls: target=%d credentials=%d upstream=%d", targetCalls.Load(), credentialCalls.Load(), roundTrips.Load())
			}
		})
	}
}

func TestWailsAssetServerDoesNotRejectPathsItDoesNotInjectionMatch(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		requestURL string
	}{
		{name: "query suffix is not part of URL path", method: http.MethodGet, requestURL: "http://native.local/api/models?next=/index.html"},
		{name: "POST bypasses Wails GET injection handling", method: http.MethodPost, requestURL: "http://native.local/api/models/"},
		{name: "HEAD bypasses Wails GET injection handling", method: http.MethodHead, requestURL: "http://native.local/api/models/"},
		{name: "backslash remains a URL path character on every host OS", method: http.MethodGet, requestURL: "http://native.local/api/models%5C"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var roundTrips atomic.Int32
			client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				roundTrips.Add(1)
				return &http.Response{
					StatusCode: http.StatusNoContent,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader("")),
					Request:    request,
				}, nil
			})}
			handler := New(
				staticTarget{profile: configstore.Profile{BaseURL: "https://enterprise.example.com"}, ok: true},
				staticCredentials{},
				client,
			)
			server, err := wailsassetserver.NewAssetServer(
				"",
				wailsassetoptions.Options{
					Assets:     fstest.MapFS{"index.html": {Data: []byte("<html><head></head><body>app</body></html>")}},
					Handler:    handler,
					Middleware: AssetDocumentGuard,
				},
				false,
				nil,
				testRuntimeAssets{},
			)
			if err != nil {
				t.Fatalf("NewAssetServer() error = %v", err)
			}

			response := httptest.NewRecorder()
			server.ServeHTTP(response, httptest.NewRequest(tt.method, tt.requestURL, nil))

			if response.Code != http.StatusNoContent {
				t.Fatalf("response = %d %q", response.Code, response.Body.String())
			}
			if roundTrips.Load() != 1 {
				t.Fatalf("upstream round trips = %d, want 1", roundTrips.Load())
			}
		})
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

func TestHandlerRejectsInvalidOrTamperedProfileOriginBeforeCredentials(t *testing.T) {
	for _, baseURL := range []string{
		"http://example.com",
		"https://example.com/tenant-path",
		"https://user:secret@example.com",
		"https://exa\u200bmple.com",
		"https://127.1",
		"https://0x7f000001",
		"https://127.0.0.1.",
		"https://１２７.０.０.１",
		"https://127。0。0。1",
	} {
		t.Run(baseURL, func(t *testing.T) {
			var credentialCalls atomic.Int32
			var roundTrips atomic.Int32
			client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				roundTrips.Add(1)
				return &http.Response{
					StatusCode: http.StatusNoContent,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader("")),
				}, nil
			})}
			handler := New(
				staticTarget{profile: configstore.Profile{BaseURL: baseURL}, ok: true},
				countingCredentials{calls: &credentialCalls},
				client,
			)
			response := httptest.NewRecorder()

			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://native.local/api/models", nil))

			assertJSONError(t, response, http.StatusServiceUnavailable)
			if credentialCalls.Load() != 0 {
				t.Fatalf("credential calls = %d", credentialCalls.Load())
			}
			if roundTrips.Load() != 0 {
				t.Fatalf("upstream round trips = %d", roundTrips.Load())
			}
			if strings.Contains(response.Body.String(), baseURL) || strings.Contains(response.Body.String(), "secret") {
				t.Fatalf("response leaked profile data: %q", response.Body.String())
			}
		})
	}
}

func TestHandlerRejectsTraversalPathsBeforeTargetLookup(t *testing.T) {
	for _, requestPath := range []string{
		"/api/../auth",
		"/api/./models",
		"/api/%2e%2e%2fauth",
	} {
		t.Run(requestPath, func(t *testing.T) {
			var targetCalls atomic.Int32
			var credentialCalls atomic.Int32
			var roundTrips atomic.Int32
			client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				roundTrips.Add(1)
				return nil, nil
			})}
			handler := New(
				countingTarget{calls: &targetCalls, profile: configstore.Profile{BaseURL: "http://localhost"}, ok: true},
				countingCredentials{calls: &credentialCalls},
				client,
			)
			response := httptest.NewRecorder()

			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://native.local"+requestPath, nil))

			assertJSONError(t, response, http.StatusNotFound)
			if targetCalls.Load() != 0 || credentialCalls.Load() != 0 || roundTrips.Load() != 0 {
				t.Fatalf("calls: target=%d credentials=%d upstream=%d", targetCalls.Load(), credentialCalls.Load(), roundTrips.Load())
			}
		})
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

func TestHandlerRefusesSameOriginAndRelativeRedirects(t *testing.T) {
	for _, location := range []string{"absolute-api", "/api/next", "/login"} {
		t.Run(location, func(t *testing.T) {
			var upstream *httptest.Server
			upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				redirectLocation := location
				if location == "absolute-api" {
					redirectLocation = upstream.URL + "/api/next"
				}
				w.Header().Set("Location", redirectLocation)
				w.WriteHeader(http.StatusTemporaryRedirect)
			}))
			defer upstream.Close()
			handler := New(
				staticTarget{profile: configstore.Profile{BaseURL: upstream.URL}, ok: true},
				staticCredentials{},
				nil,
			)
			response := httptest.NewRecorder()

			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://native.local/api/models", nil))

			assertJSONError(t, response, http.StatusBadGateway)
			if got := response.Header().Get("Location"); got != "" {
				t.Fatalf("Location = %q", got)
			}
		})
	}
}

func TestHandlerPreservesNotModifiedResponse(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "/cached-resource")
		w.WriteHeader(http.StatusNotModified)
	}))
	defer upstream.Close()
	handler := New(
		staticTarget{profile: configstore.Profile{BaseURL: upstream.URL}, ok: true},
		staticCredentials{},
		nil,
	)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://native.local/api/models", nil))

	if response.Code != http.StatusNotModified {
		t.Fatalf("status = %d, body = %q", response.Code, response.Body.String())
	}
}

func TestHandlerStripsAllUpstreamSetCookieHeaders(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Add("Set-Cookie", "session=secret-one; HttpOnly")
		w.Header().Add("Set-Cookie", "tenant=secret-two; Secure")
		_, _ = io.WriteString(w, "ok")
	}))
	defer upstream.Close()
	handler := New(
		staticTarget{profile: configstore.Profile{BaseURL: upstream.URL}, ok: true},
		staticCredentials{},
		nil,
	)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://native.local/api/models", nil))

	if response.Code != http.StatusOK || response.Body.String() != "ok" {
		t.Fatalf("response = %d %q", response.Code, response.Body.String())
	}
	if cookies := response.Header().Values("Set-Cookie"); len(cookies) != 0 {
		t.Fatalf("Set-Cookie = %q", cookies)
	}
}

func TestHandlerSuppressesEarlyHintsAndStripsCookies(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Set-Cookie", "early=secret-early")
		w.Header().Set("Link", "</app.css>; rel=preload")
		w.WriteHeader(http.StatusEarlyHints)
		w.Header().Set("Set-Cookie", "final=secret-final")
		_, _ = io.WriteString(w, "ok")
	}))
	defer upstream.Close()
	downstream := httptest.NewServer(New(
		staticTarget{profile: configstore.Profile{BaseURL: upstream.URL}, ok: true},
		staticCredentials{},
		nil,
	))
	defer downstream.Close()

	var earlyHints []http.Header
	trace := &httptrace.ClientTrace{
		Got1xxResponse: func(code int, header textproto.MIMEHeader) error {
			if code == http.StatusEarlyHints {
				earlyHints = append(earlyHints, http.Header(header).Clone())
			}
			return nil
		},
	}
	request, err := http.NewRequestWithContext(
		httptrace.WithClientTrace(t.Context(), trace),
		http.MethodGet,
		downstream.URL+"/api/models",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	response, err := downstream.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}

	if len(earlyHints) != 0 {
		t.Fatalf("early hints count = %d", len(earlyHints))
	}
	if response.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Fatalf("response = %d %q", response.StatusCode, body)
	}
	if cookies := response.Header.Values("Set-Cookie"); len(cookies) != 0 {
		t.Fatalf("final Set-Cookie = %q", cookies)
	}
}

func TestHandlerSuppressesInformationalResponsesForFirstWriteHeaderWinsWriter(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Link", "</early.css>; rel=preload")
		w.Header().Set("Set-Cookie", "early=secret-early")
		w.WriteHeader(http.StatusEarlyHints)
		w.Header().Del("Link")
		w.Header().Del("Set-Cookie")

		w.Header().Set("X-Processing", "temporary")
		w.Header().Set("Set-Cookie", "processing=secret-processing")
		w.WriteHeader(http.StatusProcessing)
		w.Header().Del("X-Processing")
		w.Header().Del("Set-Cookie")

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Final", "present")
		w.Header().Set("Set-Cookie", "final=secret-final")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()
	handler := New(
		staticTarget{profile: configstore.Profile{BaseURL: upstream.URL}, ok: true},
		staticCredentials{},
		nil,
	)
	response := newFirstWriteHeaderWins()

	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://native.local/api/models", nil))

	if response.status != http.StatusOK {
		t.Fatalf("status = %d, body = %q", response.status, response.body.String())
	}
	if response.body.String() != `{"ok":true}` {
		t.Fatalf("body = %q", response.body.String())
	}
	if got := response.header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := response.header.Get("X-Final"); got != "present" {
		t.Fatalf("X-Final = %q", got)
	}
	for _, name := range []string{"Link", "X-Processing", "Set-Cookie"} {
		if values := response.header.Values(name); len(values) != 0 {
			t.Fatalf("%s = %q", name, values)
		}
	}
}

func TestHandlerStripsSetCookieResponseTrailer(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Add("Trailer", "Set-Cookie")
		w.Header().Add("Trailer", "X-Checksum")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
		w.Header().Set("Set-Cookie", "trailer=secret-trailer")
		w.Header().Set("X-Checksum", "passed")
	}))
	defer upstream.Close()
	downstream := httptest.NewServer(New(
		staticTarget{profile: configstore.Profile{BaseURL: upstream.URL}, ok: true},
		staticCredentials{},
		nil,
	))
	defer downstream.Close()

	response, err := downstream.Client().Get(downstream.URL + "/api/models")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}

	if response.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Fatalf("response = %d %q", response.StatusCode, body)
	}
	if cookies := response.Header.Values("Set-Cookie"); len(cookies) != 0 {
		t.Fatalf("header Set-Cookie = %q", cookies)
	}
	if cookies := response.Trailer.Values("Set-Cookie"); len(cookies) != 0 {
		t.Fatalf("trailer Set-Cookie = %q", cookies)
	}
	if got := response.Trailer.Get("X-Checksum"); got != "passed" {
		t.Fatalf("X-Checksum trailer = %q", got)
	}
}

func TestHandlerSanitizesUpstreamAuthorizationFailures(t *testing.T) {
	for _, upstreamStatus := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(upstreamStatus), func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("WWW-Authenticate", `DPoP error="secret-challenge"`)
				w.Header().Set("Proxy-Authenticate", `Basic realm="secret-proxy"`)
				w.Header().Set("Set-Cookie", "session=secret-cookie")
				w.Header().Set("X-Diagnostic-ID", "secret-diagnostic")
				w.Header().Set("Location", "https://secret.example/login")
				w.Header().Add("Trailer", "X-Diagnostic-Trailer")
				w.WriteHeader(upstreamStatus)
				_, _ = io.WriteString(w, `{"secret":"upstream-detail"}`)
				w.Header().Set("X-Diagnostic-Trailer", "secret-trailer")
			}))
			defer upstream.Close()
			downstream := httptest.NewServer(New(
				staticTarget{profile: configstore.Profile{BaseURL: upstream.URL}, ok: true},
				staticCredentials{},
				nil,
			))
			defer downstream.Close()

			response, err := downstream.Client().Get(downstream.URL + "/api/models")
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}

			if response.StatusCode != http.StatusUnauthorized {
				t.Fatalf("status = %d", response.StatusCode)
			}
			if got := string(body); got != `{"error":"authorization required"}` {
				t.Fatalf("body = %q", got)
			}
			if got := response.Header.Get("Content-Type"); got != "application/json" {
				t.Fatalf("Content-Type = %q", got)
			}
			if got, want := response.Header.Get("Content-Length"), strconv.Itoa(len(`{"error":"authorization required"}`)); got != want {
				t.Fatalf("Content-Length = %q, want %q", got, want)
			}
			for name := range response.Header {
				switch name {
				case "Content-Type", "Content-Length", "Date":
				default:
					t.Fatalf("unexpected response header %s: %q", name, response.Header.Values(name))
				}
			}
			if len(response.Trailer) != 0 {
				t.Fatalf("trailers = %q", response.Trailer)
			}
			for _, secret := range []string{"secret-challenge", "secret-proxy", "secret-cookie", "secret-diagnostic", "secret.example", "secret-trailer", "upstream-detail"} {
				if strings.Contains(string(body), secret) {
					t.Fatalf("body leaked %q: %q", secret, body)
				}
				for _, values := range response.Header {
					if strings.Contains(strings.Join(values, " "), secret) {
						t.Fatalf("headers leaked %q: %q", secret, response.Header)
					}
				}
			}
		})
	}
}

func TestHandlerClosesUnauthorizedUpstreamBodyWithoutReadingIt(t *testing.T) {
	originalBody := newTrackingBody(`{"secret":"do-not-read"}`)
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			Status:           "403 Forbidden",
			StatusCode:       http.StatusForbidden,
			Header:           http.Header{"X-Diagnostic-ID": {"secret-header"}},
			Body:             originalBody,
			ContentLength:    int64(originalBody.reader.Len()),
			TransferEncoding: []string{"chunked"},
			Trailer:          http.Header{"X-Diagnostic-Trailer": {"secret-trailer"}},
			Request:          request,
		}, nil
	})}
	handler := New(
		staticTarget{profile: configstore.Profile{BaseURL: "http://localhost"}, ok: true},
		staticCredentials{},
		client,
	)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://native.local/api/models", nil))

	if response.Code != http.StatusUnauthorized || response.Body.String() != `{"error":"authorization required"}` {
		t.Fatalf("response = %d %q", response.Code, response.Body.String())
	}
	if !originalBody.closed.Load() {
		t.Fatal("original upstream body was not closed")
	}
	if reads := originalBody.reads.Load(); reads != 0 {
		t.Fatalf("original upstream body reads = %d", reads)
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

	ctx, cancel := context.WithCancel(t.Context())
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

// bridgeTarget resolves a per-profile target bridge (edge entries).
type bridgeTarget struct {
	staticTarget
	bridge string
}

func (b bridgeTarget) ProfileBridge(context.Context, string) string { return b.bridge }

// TestHandlerSendsProfileBridgeHeader: an edge entry's target bridge
// rides X-Woca-Bridge on every proxied request (v1.6 入口② — native
// clients hold no cookies, the header is the routing signal).
func TestHandlerSendsProfileBridgeHeader(t *testing.T) {
	observed := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observed <- r.Header.Get(BridgeHeader)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	withBridge := New(
		bridgeTarget{staticTarget: staticTarget{profile: configstore.Profile{ID: 1, BaseURL: upstream.URL}, ok: true}, bridge: "br_target"},
		staticCredentials{},
		nil,
	)
	response := httptest.NewRecorder()
	withBridge.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://native.local/api/sessions", nil))
	if got := <-observed; got != "br_target" {
		t.Fatalf("bridge header = %q, want br_target", got)
	}
	if response.Code != http.StatusOK {
		t.Fatalf("response = %d", response.Code)
	}

	// Direct/embedded profiles (no stored bridge) send no header.
	withoutBridge := New(
		bridgeTarget{staticTarget: staticTarget{profile: configstore.Profile{ID: 2, BaseURL: upstream.URL}, ok: true}},
		staticCredentials{},
		nil,
	)
	response2 := httptest.NewRecorder()
	withoutBridge.ServeHTTP(response2, httptest.NewRequest(http.MethodGet, "http://native.local/api/sessions", nil))
	if got := <-observed; got != "" {
		t.Fatalf("bridge header = %q, want empty for direct profiles", got)
	}
	if response2.Code != http.StatusOK {
		t.Fatalf("response = %d", response2.Code)
	}
}
