package config

import (
	"net"
	"net/http"
	"os"
	"time"
)

// modelHTTPClient builds the HTTP client used for provider API calls.
//
// Several OpenAI-compatible gateways (observed with DeepSeek's api.deepseek.com
// and similar fronts) close a keep-alive connection once one streaming
// response has been served on it, resetting the NEXT request that the Go
// connection pool dispatches over the reused connection — surfacing as
// "context canceled" a round-trip into the model call, typically on the
// second request of a tool round-trip. Disabling keep-alives gives every
// model request its own connection, trading one TLS handshake per request
// for reliable streaming.
//
// Set WOCA_KEEPALIVE_MODELS=1 to fall back to the default pooled client.
//
// No client-level Timeout is set on purpose: http.Client.Timeout covers the
// whole exchange INCLUDING reading the streaming body, so it would cut long
// SSE responses; request deadlines are handled by the provider clients.
func modelHTTPClient() *http.Client {
	if os.Getenv("WOCA_KEEPALIVE_MODELS") == "1" {
		return nil
	}
	return &http.Client{
		Transport: &http.Transport{
			DisableKeepAlives: true,
			Proxy:             http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   15 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			TLSHandshakeTimeout:   15 * time.Second,
			ResponseHeaderTimeout: 120 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
	}
}
