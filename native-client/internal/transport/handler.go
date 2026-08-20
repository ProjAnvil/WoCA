package transport

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/projanvil/woca/native-client/internal/configstore"
)

type TargetProvider interface {
	ActiveProfile(context.Context) (configstore.Profile, bool, error)
}

type CredentialProvider interface {
	Credentials(context.Context, string, *url.URL) (Credentials, error)
}

type Credentials struct {
	Scheme  string
	Session string
	Proof   string
}

const RequestIDHeader = "X-Woca-Native-Request-ID"

type handler struct {
	targets       TargetProvider
	credentials   CredentialProvider
	transport     http.RoundTripper
	cancellations *CancellationRegistry
}

func New(targets TargetProvider, credentials CredentialProvider, client *http.Client) http.Handler {
	return NewWithCancellation(targets, credentials, client, nil)
}

func NewWithCancellation(
	targets TargetProvider,
	credentials CredentialProvider,
	client *http.Client,
	cancellations *CancellationRegistry,
) http.Handler {
	transport := http.RoundTripper(defaultTransport())
	if client != nil && client.Transport != nil {
		transport = client.Transport
	}
	return &handler{
		targets: targets, credentials: credentials, transport: transport,
		cancellations: cancellations,
	}
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !isAPIPath(r.URL.Path) {
		writeJSONError(w, http.StatusNotFound, "not found")
		return
	}
	if h.cancellations != nil && h.cancellations.isClosed() {
		writeJSONError(w, http.StatusServiceUnavailable, "service unavailable")
		return
	}

	r = r.Clone(r.Context())
	r.Header = r.Header.Clone()
	requestIDs := r.Header.Values(RequestIDHeader)
	r.Header.Del(RequestIDHeader)
	if h.cancellations != nil && len(requestIDs) != 0 {
		if len(requestIDs) != 1 || !validRequestID(requestIDs[0]) {
			writeJSONError(w, http.StatusBadRequest, "invalid request")
			return
		}
		ctx, unregister, ok := h.cancellations.register(r.Context(), requestIDs[0])
		if !ok {
			writeJSONError(w, http.StatusBadRequest, "invalid request")
			return
		}
		defer unregister()
		r = r.Clone(ctx)
	}
	if h.targets == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "service unavailable")
		return
	}

	profile, ok, err := h.targets.ActiveProfile(r.Context())
	if err != nil || !ok {
		writeJSONError(w, http.StatusServiceUnavailable, "service unavailable")
		return
	}
	canonicalBaseURL, err := configstore.NormalizeBaseURL(profile.BaseURL)
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "service unavailable")
		return
	}
	target, err := url.Parse(canonicalBaseURL)
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "service unavailable")
		return
	}
	destination := cloneURL(r.URL)
	destination.Scheme = target.Scheme
	destination.Host = target.Host
	destination.User = nil

	var credentials Credentials
	if h.credentials != nil {
		credentials, err = h.credentials.Credentials(r.Context(), r.Method, destination)
		if err != nil {
			writeJSONError(w, http.StatusUnauthorized, "authorization required")
			return
		}
	}

	proxy := &httputil.ReverseProxy{
		Director: func(out *http.Request) {
			out.URL = cloneURL(destination)
			out.Host = target.Host
			stripUntrustedHeaders(out.Header)
			out.TransferEncoding = nil
			out.Trailer = nil
			if credentials.Scheme != "" && credentials.Session != "" {
				out.Header.Set("Authorization", credentials.Scheme+" "+credentials.Session)
			}
			if credentials.Proof != "" {
				out.Header.Set("DPoP", credentials.Proof)
			}
		},
		Transport:     h.transport,
		FlushInterval: -1,
		ModifyResponse: func(response *http.Response) error {
			if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
				sanitizeUnauthorizedResponse(response)
				return nil
			}
			stripResponseCookies(response.Header)
			stripResponseCookies(response.Trailer)
			if response.Body != nil {
				response.Body = &cookieStrippingBody{ReadCloser: response.Body, trailer: &response.Trailer}
			}
			if response.StatusCode >= http.StatusMultipleChoices &&
				response.StatusCode < http.StatusBadRequest &&
				response.StatusCode != http.StatusNotModified &&
				len(response.Header.Values("Location")) != 0 {
				return errors.New("upstream redirect refused")
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			writeJSONError(w, http.StatusBadGateway, "bad gateway")
		},
	}
	proxyRequest := r.Clone(r.Context())
	stripUntrustedHeaders(proxyRequest.Header)
	proxyRequest.TransferEncoding = nil
	proxyRequest.Trailer = nil
	proxy.ServeHTTP(&cookieStrippingResponseWriter{ResponseWriter: w}, proxyRequest)
}

func defaultTransport() *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
}

func stripUntrustedHeaders(headers http.Header) {
	for _, value := range headers.Values("Connection") {
		for _, name := range strings.Split(value, ",") {
			headers.Del(strings.TrimSpace(name))
		}
	}
	for _, name := range []string{
		"Authorization", "DPoP", "Cookie", "Forwarded", "Connection", "Proxy-Connection",
		"Keep-Alive", "TE", "Trailer", "Transfer-Encoding", "Upgrade", RequestIDHeader,
	} {
		headers.Del(name)
	}
	for name := range headers {
		if strings.HasPrefix(strings.ToLower(name), "x-forwarded-") {
			headers.Del(name)
		}
	}
	// A nil X-Forwarded-For entry tells ReverseProxy not to synthesize one from
	// the untrusted local request after Director returns.
	headers["X-Forwarded-For"] = nil
}

func cloneURL(source *url.URL) *url.URL {
	cloned := *source
	return &cloned
}

type cookieStrippingResponseWriter struct {
	http.ResponseWriter
}

func (w *cookieStrippingResponseWriter) WriteHeader(statusCode int) {
	stripResponseCookies(w.Header())
	if statusCode >= 100 && statusCode < 200 {
		clear(w.Header())
		return
	}
	w.ResponseWriter.WriteHeader(statusCode)
}

func (w *cookieStrippingResponseWriter) Write(body []byte) (int, error) {
	stripResponseCookies(w.Header())
	return w.ResponseWriter.Write(body)
}

func (w *cookieStrippingResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

type cookieStrippingBody struct {
	io.ReadCloser
	trailer *http.Header
}

func (b *cookieStrippingBody) Read(buffer []byte) (int, error) {
	read, err := b.ReadCloser.Read(buffer)
	if err != nil {
		stripResponseCookies(*b.trailer)
	}
	return read, err
}

func (b *cookieStrippingBody) Close() error {
	err := b.ReadCloser.Close()
	stripResponseCookies(*b.trailer)
	return err
}

func stripResponseCookies(headers http.Header) {
	for name, values := range headers {
		if strings.EqualFold(name, "Set-Cookie") || strings.EqualFold(name, http.TrailerPrefix+"Set-Cookie") {
			delete(headers, name)
			continue
		}
		if !strings.EqualFold(name, "Trailer") {
			continue
		}
		filteredValues := make([]string, 0, len(values))
		for _, value := range values {
			filteredNames := make([]string, 0, 1)
			for _, trailerName := range strings.Split(value, ",") {
				trailerName = strings.TrimSpace(trailerName)
				if trailerName != "" && !strings.EqualFold(trailerName, "Set-Cookie") {
					filteredNames = append(filteredNames, trailerName)
				}
			}
			if len(filteredNames) != 0 {
				filteredValues = append(filteredValues, strings.Join(filteredNames, ", "))
			}
		}
		if len(filteredValues) == 0 {
			delete(headers, name)
		} else {
			headers[name] = filteredValues
		}
	}
}

func sanitizeUnauthorizedResponse(response *http.Response) {
	if response.Body != nil {
		_ = response.Body.Close()
	}
	const body = `{"error":"authorization required"}`
	response.StatusCode = http.StatusUnauthorized
	response.Status = strconv.Itoa(http.StatusUnauthorized) + " " + http.StatusText(http.StatusUnauthorized)
	response.Header = make(http.Header)
	response.Header.Set("Content-Type", "application/json")
	response.Header.Set("Content-Length", strconv.Itoa(len(body)))
	response.Body = io.NopCloser(strings.NewReader(body))
	response.ContentLength = int64(len(body))
	response.TransferEncoding = nil
	response.Trailer = nil
	response.Close = false
	response.Uncompressed = false
}

func isAPIPath(requestPath string) bool {
	if requestPath != "/api" && !strings.HasPrefix(requestPath, "/api/") {
		return false
	}
	for _, segment := range strings.Split(requestPath, "/") {
		if segment == "." || segment == ".." {
			return false
		}
	}
	cleaned := path.Clean(requestPath)
	return cleaned == "/api" || strings.HasPrefix(cleaned, "/api/")
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"error":"` + message + `"}`))
}
