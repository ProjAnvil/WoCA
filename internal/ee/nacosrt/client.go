// Package nacosrt is the enterprise nacos runtime: the shared component
// every full-stack role (woca-scheduler, woca-worker, woca-edge) assembles to
// (1) register itself with nacos and keep the heartbeat, and (2) subscribe
// to the delivered configuration pack. It speaks the nacos v1 HTTP APIs
// with the standard library only — no SDK, same discipline as
// internal/ee/discovery.
//
// This package is enterprise-only: the personal edition and the bridge
// never link it. Startup is fail-closed by contract — a role that cannot
// reach nacos does not start (there is no fallback posture; the operator
// sees the error and fixes the registry).
package nacosrt

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// Client is the nacos v1 HTTP protocol surface used by the runtime: the
// config plane (publish / get / long-pulling listener) and the naming
// plane (instance register / heartbeat / deregister).
type Client struct {
	base      string // http://host:port — the nacos console port
	namespace string // optional nacos namespace (tenant)

	mu    sync.Mutex
	token string
}

// NewClient builds a client over the nacos base URL (for example
// http://nacos:8848). Credentials come from WOCA_DISCOVERY_USERNAME /
// WOCA_DISCOVERY_PASSWORD when set (Nacos auth enabled); without them the
// anonymous v1 APIs are used.
func NewClient(base string) *Client {
	return &Client{base: strings.TrimRight(base, "/")}
}

// NewClientInNamespace is NewClient pinned to one nacos namespace
// (config references carry it as the ?namespace= query).
func NewClientInNamespace(base, namespace string) *Client {
	return &Client{base: strings.TrimRight(base, "/"), namespace: namespace}
}

// Ping verifies protocol reachability over the readiness endpoint. It is
// the startup gate's probe: an error here means the role must not start.
func (c *Client) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/nacos/v1/console/health/readiness", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("nacosrt: ping %s: %w", c.base, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("nacosrt: ping %s: registry not ready (HTTP %d)", c.base, resp.StatusCode)
	}
	return nil
}

// ErrNoConfig marks an entry that does not exist (yet).
var ErrNoConfig = errors.New("nacosrt: config entry does not exist")

// Publish stores content under group/dataId. The nacos write-then-read
// settling window is real (a true return is not immediately readable), so
// callers that gate dependents on the write must read back themselves.
func (c *Client) Publish(ctx context.Context, group, dataID, content string) error {
	body := func() string {
		form := url.Values{}
		form.Set("dataId", dataID)
		form.Set("group", group)
		form.Set("content", content)
		return form.Encode()
	}()
	return c.do(ctx, http.MethodPost, func() string {
		return "/nacos/v1/cs/configs?" + c.authQuery(url.Values{}).Encode()
	}, body, func(resp *http.Response, respBody string) error {
		if respBody != "true" {
			return fmt.Errorf("publish reply: %s", respBody)
		}
		return nil
	})
}

// Get reads one config entry. ErrNoConfig distinguishes "never
// delivered" from transport failure.
func (c *Client) Get(ctx context.Context, group, dataID string) (string, error) {
	urlc := func() string {
		q := url.Values{"dataId": {dataID}, "group": {group}}
		if c.namespace != "" {
			q.Set("tenant", c.namespace)
		}
		return "/nacos/v1/cs/configs?" + c.authQuery(q).Encode()
	}
	var content string
	err := c.do(ctx, http.MethodGet, urlc, "", func(resp *http.Response, body string) error {
		if strings.Contains(body, "config data not exist") {
			return ErrNoConfig
		}
		content = body
		return nil
	})
	return content, err
}

// contentMD5 is the listener protocol's change fingerprint: nacos
// compares lowercase-hex MD5 digests (not base64).
func contentMD5(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

// Listen performs one long-pulling cycle for a single config entry. It
// blocks up to timeout waiting for a change relative to md5 and reports
// whether the entry changed.
func (c *Client) Listen(ctx context.Context, group, dataID, md5 string, timeout time.Duration) (bool, error) {
	probe := dataID + "\x02" + group + "\x02" + md5 + "\x02" + "\x01"
	form := c.authQuery(url.Values{"Listening-Configs": {probe}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/nacos/v1/cs/configs/listener", strings.NewReader(form.Encode()))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Long-Pulling-Timeout", fmt.Sprintf("%d", timeout.Milliseconds()))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("nacosrt: listener HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	// The registry answers percent-encoded entries: "dataId%02group%01".
	decoded, err := url.QueryUnescape(string(body))
	if err != nil {
		decoded = string(body) // raw control characters — same layout
	}
	for entry := range strings.SplitSeq(decoded, "\x01") {
		parts := strings.Split(entry, "\x02")
		if parts[0] == dataID && (len(parts) < 2 || parts[1] == group) {
			return true, nil
		}
	}
	return false, nil
}

// Register adds one ephemeral instance to a service. Ephemeral instances
// live by heartbeat (Beat) — nacos drops them when beats stop, which is
// exactly the lifecycle a woca role wants.
func (c *Client) Register(ctx context.Context, service, ip string, port int, metadata map[string]string) error {
	meta := "{}"
	if len(metadata) > 0 {
		if raw, err := json.Marshal(metadata); err == nil {
			meta = string(raw)
		}
	}
	form := url.Values{}
	form.Set("serviceName", service)
	form.Set("ip", ip)
	form.Set("port", fmt.Sprintf("%d", port))
	form.Set("groupName", "DEFAULT_GROUP")
	form.Set("metadata", meta)
	form.Set("ephemeral", "true")
	return c.do(ctx, http.MethodPost, func() string {
		return "/nacos/v1/ns/instance?" + c.authQuery(url.Values{}).Encode()
	}, form.Encode(), nil)
}

// Beat sends one heartbeat. It reports whether nacos still knows the
// instance; false (protocol code 20404) means the runtime must
// re-register before beating again.
func (c *Client) Beat(ctx context.Context, service, ip string, port int) (bool, error) {
	beat, _ := json.Marshal(map[string]any{"serviceName": service, "ip": ip, "port": port, "cluster": "DEFAULT", "group": "DEFAULT_GROUP"})
	form := url.Values{}
	form.Set("serviceName", service)
	form.Set("ip", ip)
	form.Set("port", fmt.Sprintf("%d", port))
	form.Set("groupName", "DEFAULT_GROUP")
	form.Set("beat", string(beat))
	known := true
	err := c.do(ctx, http.MethodPut, func() string {
		return "/nacos/v1/ns/instance/beat?" + c.authQuery(url.Values{}).Encode()
	}, form.Encode(), func(resp *http.Response, body string) error {
		var out struct {
			Code int `json:"code"`
		}
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			return fmt.Errorf("beat reply: %s", body)
		}
		known = out.Code == 10200 // 20404: instance forgotten, re-register
		return nil
	})
	return known, err
}

// Deregister removes the instance on graceful shutdown so the console
// drops it immediately instead of waiting for the heartbeat to lapse.
func (c *Client) Deregister(ctx context.Context, service, ip string, port int) error {
	form := url.Values{}
	form.Set("serviceName", service)
	form.Set("ip", ip)
	form.Set("port", fmt.Sprintf("%d", port))
	form.Set("groupName", "DEFAULT_GROUP")
	form.Set("ephemeral", "true")
	return c.do(ctx, http.MethodDelete, func() string {
		return "/nacos/v1/ns/instance?" + c.authQuery(url.Values{}).Encode()
	}, form.Encode(), nil)
}

// do issues one request; urlc builds the path+query per attempt (so a
// re-login's fresh token reaches the retry), check inspects a 200
// response (nil accepts it). A 403 triggers one re-login and a retry.
func (c *Client) do(ctx context.Context, method string, urlc func() string, body string, check func(*http.Response, string) error) error {
	try := func() (*http.Response, error) {
		var rdr io.Reader
		if body != "" {
			rdr = strings.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, c.base+urlc(), rdr)
		if err != nil {
			return nil, err
		}
		if body != "" {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		return http.DefaultClient.Do(req)
	}
	resp, err := try()
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusForbidden {
		_ = resp.Body.Close()
		if err := c.login(ctx); err != nil {
			return err
		}
		if resp, err = try(); err != nil {
			return err
		}
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	text := strings.TrimSpace(string(raw))
	if resp.StatusCode != http.StatusOK {
		// The registry answers a missing config entry with 404 + "config
		// data not exist" — that is a healthy registry, not an error.
		if resp.StatusCode == http.StatusNotFound && strings.Contains(text, "config data not exist") {
			return ErrNoConfig
		}
		return fmt.Errorf("nacosrt: %s: HTTP %d: %s", method, resp.StatusCode, text)
	}
	if check != nil {
		return check(resp, text)
	}
	return nil
}

func (c *Client) authQuery(q url.Values) url.Values {
	c.mu.Lock()
	tok := c.token
	c.mu.Unlock()
	if tok != "" {
		q.Set("accessToken", tok)
	}
	return q
}

func (c *Client) login(ctx context.Context) error {
	user := os.Getenv("WOCA_DISCOVERY_USERNAME")
	pass := os.Getenv("WOCA_DISCOVERY_PASSWORD")
	if user == "" {
		return errors.New("nacosrt: registry answered 403 and no WOCA_DISCOVERY_USERNAME/PASSWORD is set")
	}
	form := url.Values{"username": {user}, "password": {pass}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/nacos/v1/auth/login", strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		AccessToken string `json:"accessToken"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.AccessToken == "" {
		return errors.New("nacosrt: login failed")
	}
	c.mu.Lock()
	c.token = out.AccessToken
	c.mu.Unlock()
	return nil
}
