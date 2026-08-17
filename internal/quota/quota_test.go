package quota

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const fixture = `{"code":200,"msg":"Operation successful","data":{
	"level":"pro",
	"limits":[
		{"type":"TOKENS_LIMIT","unit":3,"number":5,"percentage":42,"nextResetTime":1779792169974},
		{"type":"TOKENS_LIMIT","unit":6,"number":1,"percentage":15,"nextResetTime":4102444800000},
		{"type":"TIME_LIMIT","unit":5,"number":30,"percentage":3}
	]
},"success":true}`

func TestMonitorEndpoint(t *testing.T) {
	cases := []struct {
		base string
		want string
		ok   bool
	}{
		{"https://open.bigmodel.cn/api/coding/paas/v4", "https://open.bigmodel.cn/api/monitor/usage/quota/limit", true},
		{"https://api.z.ai/api/coding/paas/v4", "https://api.z.ai/api/monitor/usage/quota/limit", true},
		{"https://api.anthropic.com", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := monitorEndpoint(c.base)
		if ok != c.ok || got != c.want {
			t.Errorf("monitorEndpoint(%q) = %q,%v want %q,%v", c.base, got, ok, c.want, c.ok)
		}
	}
}

func TestFetchParsesWindows(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "sk-test" {
			t.Errorf("Authorization = %q, want raw key sk-test", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fixture))
	}))
	defer srv.Close()

	info, err := fetchEndpoint(context.Background(), srv.URL, "sk-test", srv.Client())
	if err != nil {
		t.Fatalf("fetchEndpoint: %v", err)
	}
	if !info.Available {
		t.Fatalf("expected available, reason=%q", info.Reason)
	}
	if info.Level != "pro" {
		t.Errorf("level = %q, want pro", info.Level)
	}
	if len(info.Windows) != 3 {
		t.Fatalf("windows = %+v, want 3", info.Windows)
	}
	if w := info.WindowOf("5h"); w == nil || w.UsedPercent != 42 {
		t.Errorf("5h window = %+v", w)
	}
	if w := info.WindowOf("weekly"); w == nil || w.UsedPercent != 15 {
		t.Errorf("weekly window = %+v", w)
	}
	if w := info.WindowOf("mcp"); w == nil || w.UsedPercent != 3 {
		t.Errorf("mcp window = %+v", w)
	}
	if info.WindowOf("nope") != nil {
		t.Error("WindowOf(nope) should be nil")
	}

	out := Format(info)
	for _, want := range []string{"pro", "5小时", "每周", "58.0%", "85.0%"} {
		if !strings.Contains(out, want) {
			t.Errorf("Format output missing %q:\n%s", want, out)
		}
	}
}

func TestFetchUnavailableVariants(t *testing.T) {
	// Unknown provider and missing key report unavailable, not an error.
	info, err := Fetch(context.Background(), "", "https://api.anthropic.com", "k", nil)
	if err != nil || info.Available {
		t.Errorf("non-GLM provider: info=%+v err=%v", info, err)
	}
	info, err = Fetch(context.Background(), "glm", "https://open.bigmodel.cn/api/coding/paas/v4", "", nil)
	if err != nil || info.Available || info.Reason == "" {
		t.Errorf("missing key: info=%+v err=%v", info, err)
	}
	// Custom gateways have no quota endpoints.
	info, err = Fetch(context.Background(), "deepseek", "https://gw.example.com/v1", "k", nil)
	if err != nil || info.Available || info.Reason == "" {
		t.Errorf("deepseek gateway: info=%+v err=%v", info, err)
	}
	info, err = Fetch(context.Background(), "anthropic", "https://gw.example.com", "", nil)
	if err != nil || info.Available || info.Reason == "" {
		t.Errorf("anthropic gateway: info=%+v err=%v", info, err)
	}
	info, err = Fetch(context.Background(), "openai", "https://gw.example.com/v1", "", nil)
	if err != nil || info.Available || info.Reason == "" {
		t.Errorf("openai gateway: info=%+v err=%v", info, err)
	}

	// In-band failure envelope (HTTP 200, success=false).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"code":401,"msg":"Unauthorized","data":null,"success":false}`))
	}))
	defer srv.Close()
	info, err = fetchEndpoint(context.Background(), srv.URL, "k", srv.Client())
	if err != nil {
		t.Fatalf("fetchEndpoint: %v", err)
	}
	if info.Available || info.Reason != "Unauthorized" {
		t.Errorf("failure envelope: info=%+v", info)
	}
}

func TestDeepSeekBalance(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user/balance" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-ds" {
			t.Errorf("Authorization = %q", got)
		}
		_, _ = w.Write([]byte(`{"is_available":true,"balance_infos":[` +
			`{"currency":"CNY","total_balance":"110.35","granted_balance":"10.00","topped_up_balance":"100.35"}]}`))
	}))
	defer srv.Close()
	orig := deepSeekBalanceURL
	deepSeekBalanceURL = srv.URL + "/user/balance"
	defer func() { deepSeekBalanceURL = orig }()

	info, err := fetchDeepSeekBalance(context.Background(), "", "sk-ds", srv.Client())
	if err != nil {
		t.Fatalf("fetchDeepSeekBalance: %v", err)
	}
	if !info.Available || info.Type != TypeBalance || info.Balance != "CNY 110.35" {
		t.Errorf("info = %+v", info)
	}
	if out := Format(info); !strings.Contains(out, "账户余额：CNY 110.35") {
		t.Errorf("Format = %q", out)
	}
}

func TestOAuthWindowParsing(t *testing.T) {
	// Reset timestamps arrive as RFC3339 strings; utilization as a fraction.
	var env struct {
		Windows map[string]struct {
			Utilization any `json:"utilization"`
			ResetsAt    any `json:"resets_at"`
		} `json:"windows"`
	}
	payload := `{"windows":{"five_hour":{"utilization":0.42,"resets_at":"2026-08-16T12:00:00Z"},
		"seven_day":{"utilization":15,"resets_at":1779792169974}}}`
	if err := json.Unmarshal([]byte(payload), &env); err != nil {
		t.Fatal(err)
	}
	if got := asPercent(env.Windows["five_hour"].Utilization); got != 42 {
		t.Errorf("fraction utilization = %v, want 42", got)
	}
	if got := asPercent(env.Windows["seven_day"].Utilization); got != 15 {
		t.Errorf("percent utilization = %v, want 15", got)
	}
	if ms := parseResetMS(env.Windows["seven_day"].ResetsAt); ms != 1779792169974 {
		t.Errorf("epoch-ms reset = %d", ms)
	}
	if ms := parseResetMS(env.Windows["five_hour"].ResetsAt); ms <= 0 {
		t.Errorf("RFC3339 reset = %d", ms)
	}
}

func TestHostMatches(t *testing.T) {
	cases := []struct {
		base   string
		target string
		want   bool
	}{
		{"https://api.z.ai/api/coding/paas/v4", "z.ai", true},
		{"https://open.bigmodel.cn/v4", "bigmodel.cn", true},
		{"https://evil-z.ai.example.com/v4", "z.ai", false},
		{"https://z.ai.false.com/v4", "z.ai", false},
		{"api.z.ai", "z.ai", true},   // scheme-less
		{"", "z.ai", false},          // empty
		{"https://x.cn", "bigmodel.cn", false},
	}
	for _, c := range cases {
		if got := hostMatches(c.base, c.target); got != c.want {
			t.Errorf("hostMatches(%q, %q) = %v, want %v", c.base, c.target, got, c.want)
		}
	}
}
