package quota

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// deepSeekBalanceURL is DeepSeek's prepaid-balance endpoint (same host the
// chat API lives on); swappable in tests.
var deepSeekBalanceURL = "https://api.deepseek.com/user/balance"

// fetchDeepSeekBalance queries GET /user/balance with the chat API key:
//
//	{"is_available":true,"balance_infos":[
//	  {"currency":"CNY","total_balance":"110.35",
//	   "granted_balance":"10.00","topped_up_balance":"100.35"}]}
//
// There are no rolling windows — the whole summary is the remaining amount,
// so the Info carries a formatted Balance instead of Windows.
func fetchDeepSeekBalance(ctx context.Context, baseURL, apiKey string, client *http.Client) (Info, error) {
	info := Info{Type: TypeBalance, Source: deepSeekBalanceURL}
	if apiKey == "" {
		info.Reason = "DeepSeek API key not configured; cannot query balance"
		return info, nil
	}
	// A custom base URL only changes the host, not the path.
	endpoint := deepSeekBalanceURL
	if u := strings.TrimSpace(baseURL); u != "" && !strings.Contains(u, "api.deepseek.com") {
		info.Reason = "custom DeepSeek gateway has no known balance endpoint"
		return info, nil
	}

	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return info, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return info, fmt.Errorf("quota: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return info, err
	}
	if resp.StatusCode != http.StatusOK {
		return info, fmt.Errorf("balance endpoint returned %d", resp.StatusCode)
	}

	var env struct {
		IsAvailable  bool `json:"is_available"`
		BalanceInfos []struct {
			Currency     string          `json:"currency"`
			TotalBalance json.RawMessage `json:"total_balance"`
		} `json:"balance_infos"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return info, fmt.Errorf("quota: decode response: %w", err)
	}
	if len(env.BalanceInfos) == 0 {
		info.Reason = "balance endpoint returned no account info"
		return info, nil
	}
	if !env.IsAvailable {
		info.Reason = "account balance insufficient or unavailable"
		return info, nil
	}
	var parts []string
	for _, b := range env.BalanceInfos {
		total := strings.Trim(string(b.TotalBalance), `"`)
		if total == "" {
			continue
		}
		if b.Currency != "" {
			parts = append(parts, b.Currency+" "+total)
		} else {
			parts = append(parts, total)
		}
	}
	if len(parts) == 0 {
		info.Reason = "balance endpoint returned no amounts"
		return info, nil
	}
	info.Available = true
	info.Balance = strings.Join(parts, " · ")
	return info, nil
}
