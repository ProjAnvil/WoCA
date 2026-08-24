// Package discovery resolves service addresses from optional external
// registries for VM/bare-metal private deployments (design §12.1).
//
// The principle is zero coupling: woca never imports a registry SDK and
// never self-registers — registration and discovery are deployment-layer
// duties (K8s Service/DNS in clusters). For VM deployments the address
// *source* may be Consul or Nacos, consumed here over their plain HTTP
// APIs with the standard library only.
//
// A config value that is not a discovery reference passes through
// unchanged, so local/personal deployments are unaffected:
//
//	postgres://woca:pw@pg:5432/woca              → unchanged
//	nacos://nacos:8848/woca/woca-db.dsn          → the config entry's value
//	consul://consul:8501/config/woca/db          → the KV entry's value
//
// Values are resolved once at assembly time (startup), not watched:
// address changes roll out by restarting the process, same as a ConfigMap
// rollout in K8s. Credentials come from WOCA_DISCOVERY_USERNAME /
// WOCA_DISCOVERY_PASSWORD (Nacos login).
package discovery

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/projanvil/woca/internal/wocadb"
)

// Timeout bounds one resolution (login + fetch).
const Timeout = 10 * time.Second

// Install wires Resolve into wocadb.DSNResolver so registry-backed DSN
// references resolve at open time. Server-role entry points (woca-server,
// woca-worker, woca-config, woca-tenant) call it at startup; the personal
// edition never does, so registry DSNs fail loudly there instead of
// reaching the network.
func Install() { wocadb.DSNResolver = Resolve }

// Resolve expands a discovery reference into a concrete value. Plain values
// (no nacos:// or consul:// scheme) return as-is with no network traffic.
func Resolve(ctx context.Context, value string) (string, error) {
	value = strings.TrimSpace(value)
	u, err := url.Parse(value)
	if err != nil {
		return "", fmt.Errorf("discovery: parse %q: %w", value, err)
	}
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	switch u.Scheme {
	case "nacos":
		return resolveNacos(ctx, u)
	case "consul":
		return resolveConsul(ctx, u)
	default:
		return value, nil
	}
}

// resolveNacos reads one config entry from the Nacos v1 config API.
// The reference layout is nacos://<host>[:port]/<group>/<dataId>, with an
// optional ?namespace=<id> query.
func resolveNacos(ctx context.Context, u *url.URL) (string, error) {
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", fmt.Errorf("discovery: nacos reference wants nacos://<addr>/<group>/<dataId>, got %q", u.String())
	}
	base := "http://" + u.Host
	token, err := nacosLogin(ctx, base)
	if err != nil {
		return "", err
	}
	q := url.Values{}
	q.Set("group", parts[0])
	q.Set("dataId", parts[1])
	if ns := u.Query().Get("namespace"); ns != "" {
		q.Set("tenant", ns)
	}
	if token != "" {
		q.Set("accessToken", token)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		base+"/nacos/v1/cs/configs?"+q.Encode(), nil)
	if err != nil {
		return "", fmt.Errorf("discovery: nacos request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("discovery: nacos fetch %s: %w", u.Host, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("discovery: nacos read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("discovery: nacos fetch %s/%s: HTTP %d: %s",
			parts[0], parts[1], resp.StatusCode, strings.TrimSpace(string(body)))
	}
	value := strings.TrimSpace(string(body))
	if value == "" {
		return "", fmt.Errorf("discovery: nacos config %s/%s is empty", parts[0], parts[1])
	}
	return value, nil
}

// nacosLogin exchanges credentials for an access token when
// WOCA_DISCOVERY_USERNAME is set; unauthenticated Nacos returns "".
func nacosLogin(ctx context.Context, base string) (string, error) {
	user := os.Getenv("WOCA_DISCOVERY_USERNAME")
	if user == "" {
		return "", nil
	}
	q := url.Values{}
	q.Set("username", user)
	q.Set("password", os.Getenv("WOCA_DISCOVERY_PASSWORD"))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		base+"/nacos/v1/auth/login?"+q.Encode(), nil)
	if err != nil {
		return "", fmt.Errorf("discovery: nacos login request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("discovery: nacos login: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("discovery: nacos login read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("discovery: nacos login: HTTP %d", resp.StatusCode)
	}
	var out struct {
		AccessToken string `json:"accessToken"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.AccessToken == "" {
		return "", fmt.Errorf("discovery: nacos login: no accessToken in response")
	}
	return out.AccessToken, nil
}

// resolveConsul reads one KV entry from the Consul HTTP API. The reference
// layout is consul://<host>[:port]/<key-path>.
func resolveConsul(ctx context.Context, u *url.URL) (string, error) {
	key := strings.Trim(u.Path, "/")
	if key == "" {
		return "", fmt.Errorf("discovery: consul reference wants consul://<addr>/<key>, got %q", u.String())
	}
	// The KV body is a JSON array of entries with base64 Values.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://"+u.Host+"/v1/kv/"+key, nil)
	if err != nil {
		return "", fmt.Errorf("discovery: consul request: %w", err)
	}
	if token := os.Getenv("WOCA_DISCOVERY_TOKEN"); token != "" {
		req.Header.Set("X-Consul-Token", token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("discovery: consul fetch %s: %w", u.Host, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("discovery: consul read: %w", err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return "", fmt.Errorf("discovery: consul key %q not found", key)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("discovery: consul fetch %q: HTTP %d", key, resp.StatusCode)
	}
	var entries []struct {
		Value string `json:"Value"`
	}
	if err := json.Unmarshal(body, &entries); err != nil || len(entries) == 0 {
		return "", fmt.Errorf("discovery: consul key %q: unexpected payload", key)
	}
	raw, err := base64.StdEncoding.DecodeString(entries[0].Value)
	if err != nil {
		return "", fmt.Errorf("discovery: consul key %q: decode: %w", key, err)
	}
	value := strings.TrimSpace(string(raw))
	if value == "" {
		return "", fmt.Errorf("discovery: consul key %q is empty", key)
	}
	return value, nil
}
