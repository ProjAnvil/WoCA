// MCP OAuth 2.0 authorization: discovers the authorization server from the
// MCP server's protected-resource metadata, registers a dynamic client when
// needed, and completes an authorization-code + PKCE flow against a localhost
// callback. The resulting access token is handed to the caller (persisted as
// an encrypted secret by the harness) and injected as a Bearer header by the
// streamable-HTTP transport.
package mcp

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// OAuthFlow configures one authorization run.
type OAuthFlow struct {
	// ServerName labels the token when persisted.
	ServerName string
	// Endpoint is the MCP server URL (its origin is used for discovery).
	Endpoint string
	// CallbackPort is the localhost port for the redirect (0 = pick one).
	CallbackPort int
	// Client, OpenBrowser, and SaveToken are injectable for tests.
	Client      *http.Client
	OpenBrowser func(authURL string) error
	SaveToken   func(accessToken string) error
}

type protectedResourceMetadata struct {
	AuthorizationServers []string `json:"authorization_servers"`
}

type authorizationServerMetadata struct {
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	RegistrationEndpoint  string `json:"registration_endpoint"`
	// ErrorResponse covers the RFC 8414-style error payload.
	Error string `json:"error_description,omitempty"`
}

// RunOAuth drives discovery → (dynamic registration) → authorize → token.
// The callback server runs on localhost; the flow blocks until the user
// completes authorization in their browser or ctx expires.
func RunOAuth(ctx context.Context, flow OAuthFlow) error {
	origin, err := url.Parse(flow.Endpoint)
	if err != nil {
		return fmt.Errorf("mcp oauth: bad endpoint: %w", err)
	}
	base := origin.Scheme + "://" + origin.Host
	client := flow.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}

	authServer := base
	if meta, err := discoverProtectedResource(client, base); err == nil && len(meta.AuthorizationServers) > 0 {
		authServer = meta.AuthorizationServers[0]
	}
	meta, err := discoverAuthorizationServer(client, authServer)
	if err != nil {
		return fmt.Errorf("mcp oauth: discover authorization server: %w", err)
	}
	if meta.AuthorizationEndpoint == "" || meta.TokenEndpoint == "" {
		return fmt.Errorf("mcp oauth: authorization server metadata incomplete")
	}
	meta.AuthorizationEndpoint = resolveRef(authServer, meta.AuthorizationEndpoint)
	meta.TokenEndpoint = resolveRef(authServer, meta.TokenEndpoint)
	if meta.RegistrationEndpoint != "" {
		meta.RegistrationEndpoint = resolveRef(authServer, meta.RegistrationEndpoint)
	}

	clientID, err := dynamicRegister(ctx, client, meta.RegistrationEndpoint, flow.CallbackPort)
	if err != nil {
		return fmt.Errorf("mcp oauth: dynamic client registration: %w", err)
	}

	verifier, challenge, err := pkcePair()
	if err != nil {
		return err
	}
	state, err := randomToken(16)
	if err != nil {
		return err
	}

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", flow.CallbackPort))
	if err != nil {
		return fmt.Errorf("mcp oauth: callback listener: %w", err)
	}
	defer func() { _ = ln.Close() }()
	callbackURL := "http://" + ln.Addr().String() + "/callback"

	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("state") != state {
			http.Error(w, "state mismatch", http.StatusBadRequest)
			errCh <- fmt.Errorf("mcp oauth: state mismatch")
			return
		}
		code := q.Get("code")
		if code == "" {
			http.Error(w, "no code", http.StatusBadRequest)
			errCh <- fmt.Errorf("mcp oauth: authorization denied (%s)", q.Get("error"))
			return
		}
		fmt.Fprintln(w, "Authorization complete — you can close this tab.")
		codeCh <- code
	})}
	go func() { _ = srv.Serve(ln) }()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	authURL := meta.AuthorizationEndpoint + "?" + url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {callbackURL},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {state},
	}.Encode()

	open := flow.OpenBrowser
	if open == nil {
		open = func(u string) error { return nil }
	}
	if err := open(authURL); err != nil {
		return fmt.Errorf("mcp oauth: open browser: %w (authorize manually at %s)", err, authURL)
	}

	var code string
	select {
	case code = <-codeCh:
	case err := <-errCh:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}

	token, err := exchangeCode(client, meta.TokenEndpoint, clientID, code, callbackURL, verifier)
	if err != nil {
		return fmt.Errorf("mcp oauth: token exchange: %w", err)
	}
	if flow.SaveToken != nil {
		if err := flow.SaveToken(token); err != nil {
			return fmt.Errorf("mcp oauth: save token: %w", err)
		}
	}
	return nil
}

func discoverProtectedResource(client *http.Client, base string) (*protectedResourceMetadata, error) {
	var meta protectedResourceMetadata
	if err := fetchJSON(client, base+"/.well-known/oauth-protected-resource", &meta); err != nil {
		return nil, err
	}
	return &meta, nil
}

func discoverAuthorizationServer(client *http.Client, authServer string) (*authorizationServerMetadata, error) {
	var meta authorizationServerMetadata
	wellKnown := strings.TrimSuffix(authServer, "/") + "/.well-known/oauth-authorization-server"
	if err := fetchJSON(client, wellKnown, &meta); err != nil {
		return nil, err
	}
	return &meta, nil
}

func fetchJSON(client *http.Client, url string, out any) error {
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", url, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// dynamicRegister performs RFC 7591 dynamic client registration. An empty
// registrationEndpoint falls back to a public client with a fixed id.
func dynamicRegister(ctx context.Context, client *http.Client, registrationEndpoint string, callbackPort int) (string, error) {
	if registrationEndpoint == "" {
		return "woca-mcp-public-client", nil
	}
	payload := map[string]any{
		"client_name":                "woca",
		"grant_types":                []string{"authorization_code"},
		"redirect_uris":              []string{fmt.Sprintf("http://127.0.0.1:%d/callback", callbackPort)},
		"token_endpoint_auth_method": "none",
	}
	data, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, registrationEndpoint, strings.NewReader(string(data)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("%s: %s", registrationEndpoint, resp.Status)
	}
	var reg struct {
		ClientID string `json:"client_id"`
	}
	if err := json.Unmarshal(body, &reg); err != nil || reg.ClientID == "" {
		return "", fmt.Errorf("registration response missing client_id")
	}
	return reg.ClientID, nil
}

func exchangeCode(client *http.Client, tokenEndpoint, clientID, code, redirectURI, verifier string) (string, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {clientID},
		"code_verifier": {verifier},
	}
	resp, err := client.Post(tokenEndpoint, "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: %s: %s", tokenEndpoint, resp.Status, strings.TrimSpace(string(body)))
	}
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &tok); err != nil || tok.AccessToken == "" {
		return "", fmt.Errorf("token response missing access_token")
	}
	return tok.AccessToken, nil
}

// resolveRef absolutizes a possibly-relative metadata endpoint against the
// authorization server base.
func resolveRef(base, ref string) string {
	if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") {
		return ref
	}
	return strings.TrimSuffix(base, "/") + "/" + strings.TrimPrefix(ref, "/")
}

// pkcePair generates a verifier and its S256 challenge.
func pkcePair() (verifier, challenge string, err error) {
	verifier, err = randomToken(48)
	if err != nil {
		return "", "", err
	}
	sum := sha256Sum(verifier)
	challenge = base64.RawURLEncoding.EncodeToString(sum)
	return verifier, challenge, nil
}

func randomToken(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// McpTokenSecretPrefix namespaces MCP server tokens in the secrets store.
const McpTokenSecretPrefix = "mcp_"
