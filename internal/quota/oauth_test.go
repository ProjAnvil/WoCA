package quota

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

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
