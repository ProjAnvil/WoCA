package quota

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// homeDir is swappable in tests.
var homeDir = os.UserHomeDir

var (
	anthropicUsageURL = "https://api.anthropic.com/api/oauth/usage"
	codexUsageURL     = "https://chatgpt.com/backend-api/wham/usage"
)

// fetchAnthropicSubscription reports the Anthropic subscription usage
// (five-hour and seven-day windows) from the OAuth credentials imported from
// ~/.claude/.credentials.json, mirroring cc-switch's official-subscription
// template. The Anthropic API-key billing console has no public quota
// endpoint, so a custom gateway or missing credentials simply reports
// unavailable.
func fetchAnthropicSubscription(ctx context.Context, baseURL string) (Info, error) {
	info := Info{Type: TypePlan}
	if u := strings.TrimSpace(baseURL); u != "" && !strings.Contains(strings.ToLower(u), "api.anthropic.com") {
		info.Reason = "自定义 Anthropic 网关没有已知的额度接口"
		return info, nil
	}
	token := claudeOAuthToken()
	if token == "" {
		info.Reason = "未找到 Claude Code 登录凭据（~/.claude/.credentials.json），API Key 账单无公开接口"
		return info, nil
	}
	info.Source = anthropicUsageURL

	body, status, err := getJSON(ctx, info.Source, map[string]string{
		"Authorization":  "Bearer " + token,
		"anthropic-beta": "oauth-2025-04-20",
		"Accept":         "application/json",
		"User-Agent":     "claude-code/1.0 woca",
	})
	if err != nil {
		return info, err
	}
	if status != http.StatusOK {
		return info, fmt.Errorf("订阅用量接口返回 %d", status)
	}

	// {"windows":{"five_hour":{"utilization":0.42,"resets_at":"…"},
	//   "seven_day":{"utilization":…}}, "extra_usage":{…}}
	var env struct {
		Windows map[string]struct {
			Utilization any `json:"utilization"`
			ResetsAt    any `json:"resets_at"`
		} `json:"windows"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return info, fmt.Errorf("quota: decode response: %w", err)
	}
	info.Available = true
	for kind, label := range map[string]string{"five_hour": "5h", "seven_day": "weekly"} {
		w, ok := env.Windows[kind]
		if !ok {
			continue
		}
		info.Windows = append(info.Windows, Window{
			Kind:        label,
			UsedPercent: asPercent(w.Utilization),
			NextResetMS: parseResetMS(w.ResetsAt),
		})
	}
	if len(info.Windows) == 0 {
		info.Available = false
		info.Reason = "订阅用量接口未返回配额窗口"
	}
	return info, nil
}

// fetchCodexUsage reports the ChatGPT/Codex subscription usage from the
// OAuth tokens `codex login` stores in ~/.codex/auth.json (cc-switch's
// OpenAI-official template). API-key billing has no public endpoint.
func fetchCodexUsage(ctx context.Context, baseURL string) (Info, error) {
	info := Info{Type: TypePlan}
	if u := strings.TrimSpace(baseURL); u != "" && !strings.Contains(strings.ToLower(u), "api.openai.com") {
		info.Reason = "自定义 OpenAI 网关没有已知的额度接口"
		return info, nil
	}
	token, account := codexOAuthToken()
	if token == "" {
		info.Reason = "未找到 Codex 登录凭据（~/.codex/auth.json），API Key 账单无公开接口"
		return info, nil
	}
	info.Source = codexUsageURL

	headers := map[string]string{
		"Authorization": "Bearer " + token,
		"User-Agent":    "codex-cli",
		"Accept":        "application/json",
	}
	if account != "" {
		headers["ChatGPT-Account-Id"] = account
	}
	body, status, err := getJSON(ctx, info.Source, headers)
	if err != nil {
		return info, err
	}
	if status != http.StatusOK {
		return info, fmt.Errorf("订阅用量接口返回 %d", status)
	}

	// {"rate_limit":{"primary_window":{"used_percent":42.5,
	//   "limit_window_seconds":18000,"reset_at":"…"},
	//   "secondary_window":{…}}}
	var env struct {
		RateLimit *struct {
			PrimaryWindow   *usageWindow `json:"primary_window"`
			SecondaryWindow *usageWindow `json:"secondary_window"`
		} `json:"rate_limit"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return info, fmt.Errorf("quota: decode response: %w", err)
	}
	info.Available = true
	if env.RateLimit != nil {
		for _, w := range []struct {
			win  *usageWindow
			kind string
		}{
			{env.RateLimit.PrimaryWindow, "5h"},
			{env.RateLimit.SecondaryWindow, "weekly"},
		} {
			if w.win == nil {
				continue
			}
			info.Windows = append(info.Windows, Window{
				Kind:        w.kind,
				UsedPercent: w.win.UsedPercent,
				NextResetMS: parseResetMS(w.win.ResetAt),
			})
		}
	}
	if len(info.Windows) == 0 {
		info.Available = false
		info.Reason = "订阅用量接口未返回配额窗口"
	}
	return info, nil
}

type usageWindow struct {
	UsedPercent       float64 `json:"used_percent"`
	LimitWindowSecond int     `json:"limit_window_seconds"`
	ResetAt           any     `json:"reset_at"`
}

// claudeOAuthToken reads the Anthropic OAuth access token imported from
// ~/.claude/.credentials.json (both the claudeAiOauth and claude.ai_oauth
// spellings seen in the wild).
func claudeOAuthToken() string {
	home, err := homeDir()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(home, ".claude", ".credentials.json"))
	if err != nil {
		return ""
	}
	var creds struct {
		ClaudeAiOAuth *struct {
			AccessToken string `json:"accessToken"`
		} `json:"claudeAiOauth"`
		ClaudeAiOAuthAlt *struct {
			AccessToken string `json:"accessToken"`
		} `json:"claude.ai_oauth"`
	}
	if err := json.Unmarshal(data, &creds); err != nil {
		return ""
	}
	if creds.ClaudeAiOAuth != nil && creds.ClaudeAiOAuth.AccessToken != "" {
		return creds.ClaudeAiOAuth.AccessToken
	}
	if creds.ClaudeAiOAuthAlt != nil && creds.ClaudeAiOAuthAlt.AccessToken != "" {
		return creds.ClaudeAiOAuthAlt.AccessToken
	}
	return ""
}

// codexOAuthToken reads the ChatGPT OAuth token (and account id) codex login
// stores in ~/.codex/auth.json; it is only meaningful with auth_mode
// "chatgpt".
func codexOAuthToken() (token, account string) {
	home, err := homeDir()
	if err != nil {
		return "", ""
	}
	data, err := os.ReadFile(filepath.Join(home, ".codex", "auth.json"))
	if err != nil {
		return "", ""
	}
	var auth struct {
		AuthMode string `json:"auth_mode"`
		Tokens   *struct {
			AccessToken string `json:"access_token"`
			AccountID   string `json:"account_id"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(data, &auth); err != nil {
		return "", ""
	}
	if auth.AuthMode != "" && auth.AuthMode != "chatgpt" {
		return "", ""
	}
	if auth.Tokens == nil {
		return "", ""
	}
	return auth.Tokens.AccessToken, auth.Tokens.AccountID
}

// getJSON performs a GET with headers and returns the bounded body + status.
func getJSON(ctx context.Context, url string, headers map[string]string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("quota: request failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return body, resp.StatusCode, err
}
