package quota

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// withFakeHome swaps the swappable homeDir resolver to a temp home for the
// duration of the test.
func withFakeHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	old := homeDir
	homeDir = func() (string, error) { return home, nil }
	t.Cleanup(func() { homeDir = old })
	return home
}

func writeCreds(t *testing.T, rel, content string) {
	t.Helper()
	home := withFakeHome(t)
	p := filepath.Join(home, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeOAuthToken(t *testing.T) {
	writeCreds(t, ".claude/.credentials.json", "not json")
	if claudeOAuthToken() != "" {
		t.Fatal("bad json")
	}
	writeCreds(t, ".claude/.credentials.json", `{}`)
	if claudeOAuthToken() != "" {
		t.Fatal("empty creds")
	}
	writeCreds(t, ".claude/.credentials.json", `{"claudeAiOauth":{"accessToken":"tok-a"}}`)
	if claudeOAuthToken() != "tok-a" {
		t.Fatal("claudeAiOauth spelling")
	}
	writeCreds(t, ".claude/.credentials.json", `{"claude.ai_oauth":{"accessToken":"tok-b"}}`)
	if claudeOAuthToken() != "tok-b" {
		t.Fatal("claude.ai_oauth spelling")
	}
	// Missing home directory entries degrade to empty.
	home := withFakeHome(t)
	_ = home
	if claudeOAuthToken() != "" {
		t.Fatal("missing file should be empty")
	}
}

func TestCodexOAuthToken(t *testing.T) {
	writeCreds(t, ".codex/auth.json", "not json")
	if token, _ := codexOAuthToken(); token != "" {
		t.Fatal("bad json")
	}
	writeCreds(t, ".codex/auth.json", `{"auth_mode":"apikey","tokens":{"access_token":"t"}}`)
	if token, _ := codexOAuthToken(); token != "" {
		t.Fatal("apikey mode has no oauth token")
	}
	writeCreds(t, ".codex/auth.json", `{"auth_mode":"chatgpt"}`)
	if token, _ := codexOAuthToken(); token != "" {
		t.Fatal("missing tokens")
	}
	writeCreds(t, ".codex/auth.json", `{"auth_mode":"chatgpt","tokens":{"access_token":"ct","account_id":"acc-1"}}`)
	token, account := codexOAuthToken()
	if token != "ct" || account != "acc-1" {
		t.Fatalf("token/account = %q/%q", token, account)
	}
}

func TestFetchAnthropicSubscriptionMatrix(t *testing.T) {
	ctx := context.Background()
	withFakeHome(t) // never read the developer's real credentials

	// Custom gateway: no known endpoint.
	info, err := fetchAnthropicSubscription(ctx, "http://my-gateway.example")
	if err != nil || info.Available {
		t.Fatalf("gateway = %+v %v", info, err)
	}

	// No credentials: unavailable with a reason.
	info, err = fetchAnthropicSubscription(ctx, "")
	if err != nil || info.Available || info.Reason == "" {
		t.Fatalf("no creds = %+v %v", info, err)
	}

	// With credentials against a fake usage endpoint.
	writeCreds(t, ".claude/.credentials.json", `{"claudeAiOauth":{"accessToken":"tok"}}`)
	restore := overrideUsageServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"windows":{
			"five_hour":{"utilization":0.42,"resets_at":"2026-08-17T12:00:00Z"},
			"seven_day":{"utilization":87.5,"resets_at":1755432000}
		}}`))
	})

	if _, err := fetchAnthropicSubscription(ctx, ""); err != nil {
		t.Fatal(err)
	}
	restore()

	// No windows in an otherwise successful response.
	restore = overrideUsageServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"windows":{}}`))
	})
	info, err = fetchAnthropicSubscription(ctx, "")
	if err != nil || info.Available || info.Reason == "" {
		t.Fatalf("empty windows = %+v %v", info, err)
	}
	restore()

	// Non-200 status.
	restore = overrideUsageServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	if _, err := fetchAnthropicSubscription(ctx, ""); err == nil {
		t.Fatal("expected status error")
	}
	restore()

	// Bad JSON.
	restore = overrideUsageServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not json"))
	})
	if _, err := fetchAnthropicSubscription(ctx, ""); err == nil {
		t.Fatal("expected decode error")
	}
	restore()

	// Transport failure.
	anthropicUsageURL = "http://127.0.0.1:1/usage"
	defer func() { anthropicUsageURL = "https://api.anthropic.com/api/oauth/usage" }()
	if _, err := fetchAnthropicSubscription(ctx, ""); err == nil {
		t.Fatal("expected transport error")
	}
}

func TestFetchCodexUsageMatrix(t *testing.T) {
	ctx := context.Background()
	withFakeHome(t) // never read the developer's real credentials

	info, err := fetchCodexUsage(ctx, "http://custom-gw")
	if err != nil || info.Available {
		t.Fatalf("gateway = %+v %v", info, err)
	}
	info, err = fetchCodexUsage(ctx, "")
	if err != nil || info.Available || info.Reason == "" {
		t.Fatalf("no creds = %+v %v", info, err)
	}

	writeCreds(t, ".codex/auth.json", `{"auth_mode":"chatgpt","tokens":{"access_token":"ct","account_id":"acc-9"}}`)
	restore := overrideCodexServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("ChatGPT-Account-Id") != "acc-9" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"rate_limit":{
			"primary_window":{"used_percent":42.5,"reset_at":"2026-08-17T13:00:00Z"},
			"secondary_window":{"used_percent":10,"reset_at":1755432000000}
		}}`))
	})
	if _, err := fetchCodexUsage(ctx, ""); err != nil {
		t.Fatal(err)
	}
	restore()

	restore = overrideCodexServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	info, err = fetchCodexUsage(ctx, "")
	if err != nil || info.Available || info.Reason == "" {
		t.Fatalf("no windows = %+v %v", info, err)
	}
	restore()

	restore = overrideCodexServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	if _, err := fetchCodexUsage(ctx, ""); err == nil {
		t.Fatal("expected status error")
	}
	restore()

	restore = overrideCodexServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("nope"))
	})
	if _, err := fetchCodexUsage(ctx, ""); err == nil {
		t.Fatal("expected decode error")
	}
	restore()

	codexUsageURL = "http://127.0.0.1:1/usage"
	defer func() { codexUsageURL = "https://chatgpt.com/backend-api/wham/usage" }()
	if _, err := fetchCodexUsage(ctx, ""); err == nil {
		t.Fatal("expected transport error")
	}
}

func TestGetJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Probe") != "yes" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	body, status, err := getJSON(context.Background(), srv.URL, map[string]string{"X-Probe": "yes"})
	if err != nil || status != http.StatusOK || len(body) == 0 {
		t.Fatalf("getJSON = %d %v %s", status, err, body)
	}
	if _, _, err := getJSON(context.Background(), srv.URL+"/x", nil); err != nil {
		t.Logf("400 path err=%v", err)
	}
	// An unparsable URL errors immediately.
	if _, _, err := getJSON(context.Background(), "http://[::1]:namedport", nil); err == nil {
		t.Fatal("expected url error")
	}
}

// overrideUsageServer points anthropicUsageURL at a local server and returns
// a restore function.
func overrideUsageServer(t *testing.T, h http.HandlerFunc) func() {
	t.Helper()
	srv := httptest.NewServer(h)
	old := anthropicUsageURL
	anthropicUsageURL = srv.URL + "/usage"
	return func() {
		anthropicUsageURL = old
		srv.Close()
	}
}

func overrideCodexServer(t *testing.T, h http.HandlerFunc) func() {
	t.Helper()
	srv := httptest.NewServer(h)
	old := codexUsageURL
	codexUsageURL = srv.URL + "/usage"
	return func() {
		codexUsageURL = old
		srv.Close()
	}
}

func TestFetchEndpointMatrix(t *testing.T) {
	ctx := context.Background()

	mk := func(body string, status int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "raw-key" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}))
	}

	// Success with every window kind plus an unknown type that is skipped.
	pct := 42.0
	srv := mk(`{"success":true,"data":{"level":"pro","limits":[
		{"type":"TOKENS_LIMIT","unit":3,"number":5,"percentage":42,"nextResetTime":1755432000000},
		{"type":"TOKENS_LIMIT","unit":6,"number":1,"nextResetTime":1755432000000},
		{"type":"TIME_LIMIT","unit":2,"percentage":`+jsonNum(pct)+`,"nextResetTime":1755432000000},
		{"type":"MYSTERY","unit":9}
	]}}`, http.StatusOK)
	info, err := fetchEndpoint(ctx, srv.URL, "raw-key", nil)
	if err != nil || !info.Available || info.Level != "pro" {
		t.Fatalf("success = %+v %v", info, err)
	}
	if len(info.Windows) != 3 {
		t.Fatalf("windows = %+v", info.Windows)
	}
	if info.WindowOf("5h") == nil || info.WindowOf("weekly") == nil || info.WindowOf("mcp") == nil {
		t.Fatalf("kinds = %+v", info.Windows)
	}
	srv.Close()

	// Limits present but none matching a known kind.
	srv = mk(`{"success":true,"data":{"level":"pro","limits":[{"type":"MYSTERY"}]}}`, http.StatusOK)
	info, err = fetchEndpoint(ctx, srv.URL, "raw-key", nil)
	if err != nil || info.Available || info.Reason == "" {
		t.Fatalf("unknown kinds = %+v %v", info, err)
	}
	srv.Close()

	// success=false with and without a message.
	srv = mk(`{"success":false,"msg":"rate limited"}`, http.StatusOK)
	info, err = fetchEndpoint(ctx, srv.URL, "raw-key", nil)
	if err != nil || info.Available || info.Reason != "rate limited" {
		t.Fatalf("msg = %+v %v", info, err)
	}
	srv.Close()
	srv = mk(`{"success":false}`, http.StatusOK)
	info, err = fetchEndpoint(ctx, srv.URL, "raw-key", nil)
	if err != nil || info.Reason == "" {
		t.Fatalf("default reason = %+v %v", info, err)
	}
	srv.Close()

	// Non-200 and decode errors.
	srv = mk("", http.StatusBadGateway)
	if _, err := fetchEndpoint(ctx, srv.URL, "raw-key", nil); err == nil {
		t.Fatal("expected status error")
	}
	srv.Close()
	srv = mk("not json", http.StatusOK)
	if _, err := fetchEndpoint(ctx, srv.URL, "raw-key", nil); err == nil {
		t.Fatal("expected decode error")
	}
	srv.Close()

	// Transport failure.
	if _, err := fetchEndpoint(ctx, "http://127.0.0.1:1/e", "raw-key", nil); err == nil {
		t.Fatal("expected transport error")
	}
	// Bad endpoint URL.
	if _, err := fetchEndpoint(ctx, "http://[::1]:namedport", "raw-key", nil); err == nil {
		t.Fatal("expected url error")
	}
}

func jsonNum(f float64) string {
	b, _ := json.Marshal(f)
	return string(b)
}

func TestFetchDispatch(t *testing.T) {
	ctx := context.Background()

	// Empty/glm provider with a monitor host and no key.
	info, err := Fetch(ctx, "", "https://open.bigmodel.cn/api/anthropic", "", nil)
	if err != nil || info.Available || info.Reason == "" {
		t.Fatalf("glm no key = %+v %v", info, err)
	}
	// Unknown provider without a monitor host.
	for _, p := range []string{"glm", "", "other"} {
		info, err := Fetch(ctx, p, "https://example.com", "", nil)
		if err != nil || info.Available || info.Reason == "" {
			t.Fatalf("Fetch(%q) = %+v %v", p, info, err)
		}
	}
}

func TestDeepSeekBalanceMatrix(t *testing.T) {
	ctx := context.Background()
	old := deepSeekBalanceURL
	defer func() { deepSeekBalanceURL = old }()

	mk := func(body string, status int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}))
	}
	deepSeekBalanceURL = mk(`{"is_available":true,"balance_infos":[
		{"currency":"CNY","total_balance":"110.35"},
		{"total_balance":0.5},
		{"currency":"USD"},
		{}
	]}`, http.StatusOK).URL
	info, err := Fetch(ctx, "deepseek", "https://api.deepseek.com", "ds-key", nil)
	if err != nil || !info.Available || info.Balance != "CNY 110.35 · 0.5" {
		t.Fatalf("balance = %+v %v", info, err)
	}

	deepSeekBalanceURL = mk(`{"is_available":false,"balance_infos":[{"currency":"CNY","total_balance":1}]}`, http.StatusOK).URL
	info, err = Fetch(ctx, "deepseek", "https://api.deepseek.com", "ds-key", nil)
	if err != nil || info.Available || info.Reason == "" {
		t.Fatalf("unavailable = %+v %v", info, err)
	}

	deepSeekBalanceURL = mk(`{"balance_infos":[]}`, http.StatusOK).URL
	info, err = Fetch(ctx, "deepseek", "https://api.deepseek.com", "ds-key", nil)
	if err != nil || info.Reason == "" {
		t.Fatalf("no infos = %+v %v", info, err)
	}

	deepSeekBalanceURL = mk(`{"balance_infos":[{"currency":"CNY"}]}`, http.StatusOK).URL
	info, err = Fetch(ctx, "deepseek", "https://api.deepseek.com", "ds-key", nil)
	if err != nil || info.Reason == "" {
		t.Fatalf("no amounts = %+v %v", info, err)
	}

	deepSeekBalanceURL = mk("", http.StatusTooManyRequests).URL
	if _, err := Fetch(ctx, "deepseek", "https://api.deepseek.com", "ds-key", nil); err == nil {
		t.Fatal("expected status error")
	}

	deepSeekBalanceURL = mk("garbage", http.StatusOK).URL
	if _, err := Fetch(ctx, "deepseek", "https://api.deepseek.com", "ds-key", nil); err == nil {
		t.Fatal("expected decode error")
	}

	deepSeekBalanceURL = "http://127.0.0.1:1/balance"
	if _, err := Fetch(ctx, "deepseek", "https://api.deepseek.com", "ds-key", nil); err == nil {
		t.Fatal("expected transport error")
	}
}

func TestFormatAndHelpers(t *testing.T) {
	if got := Format(Info{Available: false, Reason: "无接口"}); !strings.Contains(got, "套餐额度不可用") {
		t.Fatalf("unavailable format = %q", got)
	}
	if got := Format(Info{Available: true, Type: TypeBalance, Balance: "CNY 1.00"}); !strings.Contains(got, "账户余额：CNY 1.00") {
		t.Fatalf("balance format = %q", got)
	}
	plan := Format(Info{
		Available: true,
		Level:     "max",
		Windows: []Window{
			{Kind: "5h", UsedPercent: 120, NextResetMS: time.Now().Add(time.Hour).UnixMilli()},
			{Kind: "weekly", UsedPercent: 30},
			{Kind: "mcp", UsedPercent: 10, NextResetMS: time.Now().Add(-time.Hour).UnixMilli()},
		},
	})
	if !strings.Contains(plan, "GLM Coding Plan（max）") || !strings.Contains(plan, "0.0%（已用 120.0%") || !strings.Contains(plan, "即将") {
		t.Fatalf("plan format = %q", plan)
	}
	plain := Format(Info{Available: true, Windows: []Window{{Kind: "5h", UsedPercent: 50}}})
	if !strings.Contains(plain, "套餐剩余额度") {
		t.Fatalf("plain format = %q", plain)
	}

	for _, c := range []struct {
		kind, want string
	}{{"5h", "5小时"}, {"weekly", "每周"}, {"mcp", "MCP"}, {"x", "x"}} {
		if got := windowLabel(c.kind); got != c.want {
			t.Errorf("windowLabel(%q) = %q", c.kind, got)
		}
	}

	// parseResetMS spellings.
	if parseResetMS("2026-08-17T12:00:00Z") == 0 {
		t.Error("RFC3339 should parse")
	}
	if parseResetMS("2026-08-17T12:00:00+0800") == 0 {
		t.Error("no-colon offset should parse")
	}
	if parseResetMS("2026-08-17 12:00:00") == 0 {
		t.Error("space layout should parse")
	}
	if parseResetMS("garbage") != 0 || parseResetMS(42) != 0 || parseResetMS(nil) != 0 {
		t.Error("unusable values should be zero")
	}
	if got := parseResetMS(1_700_000_000.0); got != 1_700_000_000_000 {
		t.Errorf("epoch seconds = %d", got)
	}
	if got := parseResetMS(1_700_000_000_000.0); got != 1_700_000_000_000 {
		t.Errorf("epoch millis = %d", got)
	}
	if parseResetMS(-5.0) != 0 || parseResetMS(0.0) != 0 {
		t.Error("non-positive should be zero")
	}

	if asPercent(0.42) != 42 || asPercent(87.5) != 87.5 || asPercent("x") != 0 || asPercent(1) != 0 || asPercent(1.0) != 100 {
		t.Error("asPercent")
	}
}

func TestUnavailableReasonAllProviders(t *testing.T) {
	for _, p := range []string{"anthropic", "openai", "glm", "", "mystery"} {
		if got := unavailableReason(p); got == "" {
			t.Errorf("unavailableReason(%q) empty", p)
		}
	}
}
