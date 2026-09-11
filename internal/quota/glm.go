package quota

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// hostMatches reports whether baseURL's host is target or a subdomain of
// it. Substring sniffing would misjudge hosts like evil-z.ai.example.com.
func hostMatches(baseURL, target string) bool {
	raw := strings.TrimSpace(baseURL)
	if raw == "" {
		return false
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	target = strings.ToLower(target)
	return host == target || strings.HasSuffix(host, "."+target)
}

// monitorEndpoint maps a chat-completion base URL to the GLM quota monitor
// endpoint. ok is false for providers without a known monitor.
func monitorEndpoint(baseURL string) (string, bool) {
	switch {
	case hostMatches(baseURL, "bigmodel.cn"):
		return "https://open.bigmodel.cn/api/monitor/usage/quota/limit", true
	case hostMatches(baseURL, "z.ai"):
		return "https://api.z.ai/api/monitor/usage/quota/limit", true
	default:
		return "", false
	}
}

// fetchEndpoint performs the GLM monitor request and decodes the envelope:
//
//	{"code":200,"msg":"…","success":true,"data":{"level":"pro","limits":[
//	  {"type":"TOKENS_LIMIT","unit":3,"number":5,"percentage":42,"nextResetTime":…},
//	  {"type":"TOKENS_LIMIT","unit":6,"number":1,"percentage":15,…},
//	  {"type":"TIME_LIMIT",…}]}}
//
// percentage is the share already used. The undocumented unit codes seen in
// the wild: 3 = the 5-hour window, 6 = weekly; TIME_LIMIT is the monthly MCP
// tool lane.
func fetchEndpoint(ctx context.Context, endpoint, apiKey string, client *http.Client) (Info, error) {
	if client == nil {
		client = http.DefaultClient
	}
	info := Info{Type: TypePlan, Source: endpoint}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return info, err
	}
	// The monitor API takes the raw key; a "Bearer " prefix gets a 401.
	req.Header.Set("Authorization", apiKey)
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
		return info, fmt.Errorf("额度接口返回 %d", resp.StatusCode)
	}

	var env struct {
		Success bool   `json:"success"`
		Msg     string `json:"msg"`
		Data    *struct {
			Level  string `json:"level"`
			Limits []struct {
				Type          string   `json:"type"`
				Unit          int      `json:"unit"`
				Number        int      `json:"number"`
				Percentage    *float64 `json:"percentage"`
				NextResetTime int64    `json:"nextResetTime"`
			} `json:"limits"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return info, fmt.Errorf("quota: decode response: %w", err)
	}
	if !env.Success || env.Data == nil {
		reason := env.Msg
		if reason == "" {
			reason = "额度接口返回失败"
		}
		info.Reason = reason
		return info, nil
	}

	info.Available = true
	info.Level = env.Data.Level
	for _, l := range env.Data.Limits {
		w := Window{NextResetMS: l.NextResetTime}
		if l.Percentage != nil {
			w.UsedPercent = *l.Percentage
		}
		switch {
		case l.Type == "TIME_LIMIT":
			w.Kind = "mcp"
		case l.Type == "TOKENS_LIMIT" && l.Unit == 3:
			w.Kind = "5h"
		case l.Type == "TOKENS_LIMIT":
			w.Kind = "weekly"
		default:
			continue
		}
		info.Windows = append(info.Windows, w)
	}
	if len(info.Windows) == 0 {
		info.Available = false
		info.Reason = "额度接口未返回配额窗口"
	}
	return info, nil
}
