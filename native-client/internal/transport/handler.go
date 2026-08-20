package transport

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
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

type handler struct {
	targets     TargetProvider
	credentials CredentialProvider
	transport   http.RoundTripper
}

func New(targets TargetProvider, credentials CredentialProvider, client *http.Client) http.Handler {
	transport := http.RoundTripper(defaultTransport())
	if client != nil && client.Transport != nil {
		transport = client.Transport
	}
	return &handler{targets: targets, credentials: credentials, transport: transport}
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !isAPIPath(r.URL.Path) {
		writeJSONError(w, http.StatusNotFound, "not found")
		return
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
			response.Header.Del("Set-Cookie")
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
	proxy.ServeHTTP(w, proxyRequest)
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
		"Keep-Alive", "TE", "Trailer", "Transfer-Encoding", "Upgrade",
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
