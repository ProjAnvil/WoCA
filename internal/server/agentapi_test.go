package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/rs/zerolog"

	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/quota"
)

func newAgentAPIServer(t *testing.T, cfg config.Config, setThinking func(int) error, quotaFn func(ctx context.Context) (quota.Info, error)) (*Server, string) {
	t.Helper()
	s := New(Config{
		Agent:             newTestAgent(t),
		Static:            fstest.MapFS{},
		Logger:            zerolog.Nop(),
		CfgLive:           func() config.Config { return cfg },
		SetThinkingBudget: setThinking,
		Quota:             quotaFn,
	})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts.URL
}

func TestAgentStateAndPermissionMode(t *testing.T) {
	_, url := newAgentAPIServer(t, config.Config{
		Provider:       config.ProviderAnthropic,
		Model:          "claude-sonnet-4-5",
		ThinkingBudget: config.ThinkingBudgetMedium,
	}, nil, nil)

	// GET reports the live state, including the provider template's
	// thinking capability for the model.
	resp, err := http.Get(url + "/api/agent")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var state struct {
		Provider       string `json:"provider"`
		Model          string `json:"model"`
		PermissionMode string `json:"permission_mode"`
		Thinking       struct {
			Level     string   `json:"level"`
			Budget    int      `json:"budget"`
			Supported bool     `json:"supported"`
			Levels    []string `json:"levels"`
		} `json:"thinking"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&state); err != nil {
		t.Fatal(err)
	}
	if state.PermissionMode != "default" || state.Thinking.Level != "medium" || state.Thinking.Budget != config.ThinkingBudgetMedium {
		t.Fatalf("unexpected state: %+v", state)
	}
	if !state.Thinking.Supported || len(state.Thinking.Levels) != 3 || state.Thinking.Levels[0] != "low" {
		t.Fatalf("thinking capability for claude-sonnet-4-5: %+v", state.Thinking)
	}

	// POST switches the mode; invalid modes are rejected.
	for _, mode := range []string{"acceptEdits", "bypassPermissions", "readOnly", "plan", "default"} {
		resp, err := http.Post(url+"/api/agent/permissions", "application/json", jsonReader(map[string]string{"mode": mode}))
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("set mode %s: status %d", mode, resp.StatusCode)
		}
		resp.Body.Close()
	}
	resp, err = http.Post(url+"/api/agent/permissions", "application/json", jsonReader(map[string]string{"mode": "yolo"}))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid mode: status %d, want 400", resp.StatusCode)
	}

	// The final POST left the agent back on default.
	resp, err = http.Get(url + "/api/agent")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_ = json.NewDecoder(resp.Body).Decode(&state)
	if state.PermissionMode != "default" {
		t.Fatalf("mode after reset = %q", state.PermissionMode)
	}
}

func TestSetThinkingLevels(t *testing.T) {
	got := -1
	_, url := newAgentAPIServer(t, config.Config{Provider: config.ProviderAnthropic, Model: "claude-sonnet-4-5"}, func(b int) error {
		got = b
		return nil
	}, nil)

	post := func(body string) int {
		resp, err := http.Post(url+"/api/agent/thinking", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	if code := post(`{"level":"high"}`); code != http.StatusOK || got != config.MaxThinkingBudget {
		t.Fatalf("high: status %d budget %d", code, got)
	}
	if code := post(`{"level":"off"}`); code != http.StatusOK || got != 0 {
		t.Fatalf("off: status %d budget %d", code, got)
	}
	if code := post(`{"budget":4096}`); code != http.StatusOK || got != 4096 {
		t.Fatalf("budget: status %d budget %d", code, got)
	}
	if code := post(`{"level":"nope"}`); code != http.StatusBadRequest {
		t.Fatalf("bad level: status %d", code)
	}
	if code := post(`{"budget":100}`); code != http.StatusBadRequest {
		t.Fatalf("too-small budget: status %d", code)
	}
	if code := post(`{}`); code != http.StatusBadRequest {
		t.Fatalf("empty body: status %d", code)
	}

	// A model without a thinking mode rejects a non-zero budget…
	_, url2 := newAgentAPIServer(t, config.Config{Provider: config.ProviderOpenAI, Model: "gpt-4o"}, func(b int) error { return nil }, nil)
	resp, err := http.Post(url2+"/api/agent/thinking", "application/json", strings.NewReader(`{"level":"low"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("gpt-4o thinking: status %d, want 400", resp.StatusCode)
	}
	// …while the OpenAI reasoning family maps levels onto reasoning_effort.
	got2 := -1
	_, url3 := newAgentAPIServer(t, config.Config{Provider: config.ProviderOpenAI, Model: "gpt-5.1"}, func(b int) error {
		got2 = b
		return nil
	}, nil)
	resp3, err := http.Post(url3+"/api/agent/thinking", "application/json", strings.NewReader(`{"level":"high"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusOK || got2 != config.MaxThinkingBudget {
		t.Fatalf("gpt-5.1 thinking high: status %d budget %d", resp3.StatusCode, got2)
	}
}

func TestQuotaEndpoint(t *testing.T) {
	info := quota.Info{Available: true, Level: "pro", Windows: []quota.Window{
		{Kind: "5h", UsedPercent: 42},
	}}
	_, url := newAgentAPIServer(t, config.Config{Model: "glm-5.3"}, nil, func(ctx context.Context) (quota.Info, error) {
		return info, nil
	})

	resp, err := http.Get(url + "/api/quota")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Quota   quota.Info `json:"quota"`
		Session struct {
			Model string `json:"model"`
		} `json:"session"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if !out.Quota.Available || out.Quota.Level != "pro" || len(out.Quota.Windows) != 1 {
		t.Fatalf("quota = %+v", out.Quota)
	}
	if out.Session.Model != "glm-5.3" {
		t.Fatalf("session = %+v", out.Session)
	}

	// A nil quota callback degrades to unavailable with session usage intact.
	_, url2 := newAgentAPIServer(t, config.Config{Model: "glm-5.3"}, nil, nil)
	resp2, err := http.Get(url2 + "/api/quota")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var out2 struct {
		Quota   quota.Info     `json:"quota"`
		Session map[string]any `json:"session"`
	}
	_ = json.NewDecoder(resp2.Body).Decode(&out2)
	if out2.Quota.Available || out2.Session["model"] != "glm-5.3" {
		t.Fatalf("fallback quota = %+v session=%+v", out2.Quota, out2.Session)
	}
}

func jsonReader(v any) *strings.Reader {
	b, _ := json.Marshal(v)
	return strings.NewReader(string(b))
}

func TestAgentAPIDegradedAndValidation(t *testing.T) {
	// No agent wired: every endpoint reports 503 rather than panicking.
	bare := New(Config{Static: fstest.MapFS{}, Logger: zerolog.Nop()})
	ts := httptest.NewServer(bare.Handler())
	t.Cleanup(ts.Close)

	// GET /api/agent without an agent is a 503…
	agentResp, err := http.Get(ts.URL + "/api/agent")
	if err != nil {
		t.Fatal(err)
	}
	agentResp.Body.Close()
	if agentResp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("GET /api/agent without agent: status %d, want 503", agentResp.StatusCode)
	}
	// …and so are the two POST endpoints.
	for _, path := range []string{"/api/agent/permissions", "/api/agent/thinking"} {
		resp, err := http.Post(ts.URL+path, "application/json", strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("POST %s without agent: status %d, want 503", path, resp.StatusCode)
		}
	}
	// GET /api/quota still answers, with an empty session map.
	resp, err := http.Get(ts.URL + "/api/quota")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var q struct {
		Session map[string]any `json:"session"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&q); err != nil {
		t.Fatal(err)
	}
	if q.Session == nil || len(q.Session) != 0 {
		t.Fatalf("session without agent = %+v, want empty non-nil", q.Session)
	}

	// An agent without a live-config closure yields zero-valued provider
	// state (the embedder path).
	s := New(Config{Agent: newTestAgent(t), Static: fstest.MapFS{}, Logger: zerolog.Nop()})
	ts2 := httptest.NewServer(s.Handler())
	t.Cleanup(ts2.Close)
	resp2, err := http.Get(ts2.URL + "/api/agent")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var state agentState
	if err := json.NewDecoder(resp2.Body).Decode(&state); err != nil {
		t.Fatal(err)
	}
	if state.Provider != "" || state.Model != "" || state.Thinking.Supported {
		t.Fatalf("zero config state = %+v", state)
	}

	// Malformed bodies are 400s (the thinking variant needs a hook wired so
	// the decode runs before the availability check).
	bad, err := http.Post(ts2.URL+"/api/agent/permissions", "application/json", strings.NewReader(`{bad`))
	if err != nil {
		t.Fatal(err)
	}
	bad.Body.Close()
	if bad.StatusCode != http.StatusBadRequest {
		t.Fatalf("permissions bad JSON: status %d, want 400", bad.StatusCode)
	}

	// The thinking endpoint without a persistence hook is 503…
	noHook := New(Config{
		Agent:  newTestAgent(t),
		Static: fstest.MapFS{}, Logger: zerolog.Nop(),
		CfgLive: func() config.Config {
			return config.Config{Provider: config.ProviderAnthropic, Model: "claude-sonnet-4-5"}
		},
	})
	ts3 := httptest.NewServer(noHook.Handler())
	t.Cleanup(ts3.Close)
	resp3, err := http.Post(ts3.URL+"/api/agent/thinking", "application/json", strings.NewReader(`{"level":"low"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("thinking without hook: status %d, want 503", resp3.StatusCode)
	}

	// …and a failing hook surfaces as 500.
	failing := New(Config{
		Agent:  newTestAgent(t),
		Static: fstest.MapFS{}, Logger: zerolog.Nop(),
		CfgLive: func() config.Config {
			return config.Config{Provider: config.ProviderAnthropic, Model: "claude-sonnet-4-5"}
		},
		SetThinkingBudget: func(int) error { return errors.New("persist failed") },
	})
	ts4 := httptest.NewServer(failing.Handler())
	t.Cleanup(ts4.Close)
	resp4, err := http.Post(ts4.URL+"/api/agent/thinking", "application/json", strings.NewReader(`{"level":"low"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp4.Body.Close()
	if resp4.StatusCode != http.StatusInternalServerError {
		t.Fatalf("thinking hook error: status %d, want 500", resp4.StatusCode)
	}

	// A malformed thinking body fails validation before the hook runs.
	bad2, err := http.Post(ts4.URL+"/api/agent/thinking", "application/json", strings.NewReader(`{bad`))
	if err != nil {
		t.Fatal(err)
	}
	bad2.Body.Close()
	if bad2.StatusCode != http.StatusBadRequest {
		t.Fatalf("thinking bad JSON: status %d, want 400", bad2.StatusCode)
	}
}

func TestQuotaFetchError(t *testing.T) {
	_, url := newAgentAPIServer(t, config.Config{Model: "glm-5.3"}, nil, func(ctx context.Context) (quota.Info, error) {
		return quota.Info{}, errors.New("quota backend down")
	})
	resp, err := http.Get(url + "/api/quota")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Quota struct {
			Available bool   `json:"available"`
			Reason    string `json:"reason"`
		} `json:"quota"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Quota.Available || !strings.Contains(out.Quota.Reason, "quota backend down") {
		t.Fatalf("quota error fallback = %+v", out.Quota)
	}
}
