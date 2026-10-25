// Package quota queries per-provider account usage — the quota/balance APIs
// each provider template knows about (GLM Coding Plan monitor, DeepSeek
// balance, the Anthropic OAuth subscription usage, the Codex/ChatGPT usage
// endpoint) — and turns it into a display-ready summary used by the chat UI's
// quota chip and /quota command. Providers without a known quota endpoint
// report unavailable; callers fall back to session usage.
package quota

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Quota info kinds.
const (
	// TypePlan is a subscription-plan quota reported as used percentages
	// over rolling windows (GLM Coding Plan, Claude subscription).
	TypePlan = "plan"
	// TypeBalance is a prepaid balance reported as a currency amount
	// (DeepSeek).
	TypeBalance = "balance"
)

// Window is one quota window reported by the provider.
type Window struct {
	// Kind labels the window: "5h" (rolling five-hour token window),
	// "weekly", or "mcp" (monthly tool-call lane).
	Kind string `json:"kind"`
	// UsedPercent is the share of the window already spent (0-100).
	UsedPercent float64 `json:"used_percent"`
	// NextResetMS is the epoch-millisecond reset time (0 when unknown).
	NextResetMS int64 `json:"next_reset_ms"`
}

// Info is the provider quota summary. Available is false (with a Reason)
// when the provider has no quota endpoint or the account can't be queried;
// that is a normal outcome, not an error.
type Info struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
	Source    string `json:"source,omitempty"`
	// Type distinguishes plan quotas (Windows) from prepaid balances
	// (Balance); empty defaults to TypePlan.
	Type    string   `json:"type,omitempty"`
	Level   string   `json:"level,omitempty"`
	Windows []Window `json:"windows,omitempty"`
	// Balance is the formatted prepaid amount for TypeBalance providers
	// (e.g. "CNY 110.35").
	Balance string `json:"balance,omitempty"`
}

// WindowOf returns the first window of the given kind (nil when absent).
func (i Info) WindowOf(kind string) *Window {
	for _, w := range i.Windows {
		if w.Kind == kind {
			return &w
		}
	}
	return nil
}

// Fetch queries the provider's account usage with the credentials the chat
// model uses (the provider template dispatch). An unavailable provider yields
// Info{Available:false} with a nil error; transport and API failures are
// errors. An empty provider falls back to base-URL sniffing for the GLM
// monitor (legacy callers).
func Fetch(ctx context.Context, provider, baseURL, apiKey string, client *http.Client) (Info, error) {
	switch provider {
	case "deepseek":
		return fetchDeepSeekBalance(ctx, baseURL, apiKey, client)
	case "anthropic":
		return fetchAnthropicSubscription(ctx, baseURL)
	case "openai":
		return fetchCodexUsage(ctx, baseURL)
	default: // "", "glm"
		if endpoint, ok := monitorEndpoint(baseURL); ok {
			if apiKey == "" {
				return Info{Available: false, Reason: "API key not configured; cannot query plan quota"}, nil
			}
			return fetchEndpoint(ctx, endpoint, apiKey, client)
		}
		return Info{Available: false, Reason: unavailableReason(provider)}, nil
	}
}

func unavailableReason(provider string) string {
	switch provider {
	case "anthropic":
		return "Anthropic API-key billing has no public query endpoint (usage shows after signing in with the official subscription)"
	case "openai":
		return "OpenAI API-key billing has no public query endpoint (usage shows after signing in with the Codex subscription)"
	case "glm", "":
		return "this provider has no known quota endpoint (GLM Coding Plan supported: bigmodel.cn / z.ai)"
	default:
		return fmt.Sprintf("provider %s has no known quota endpoint", provider)
	}
}

// Format renders Info for terminals (/quota, the TUI).
func Format(info Info) string {
	if !info.Available {
		return "plan quota unavailable: " + info.Reason + "\n(run /cost to see this session's usage)\n"
	}
	if info.Type == TypeBalance {
		var b strings.Builder
		b.WriteString("Account balance: " + info.Balance + "\n")
		return b.String()
	}
	var b strings.Builder
	if info.Level != "" {
		fmt.Fprintf(&b, "GLM Coding Plan (%s) remaining quota:\n", info.Level)
	} else {
		b.WriteString("Remaining plan quota:\n")
	}
	for _, w := range info.Windows {
		remaining := 100 - w.UsedPercent
		if remaining < 0 {
			remaining = 0
		}
		fmt.Fprintf(&b, "  %-6s remaining %5.1f%% (used %.1f%%", windowLabel(w.Kind), remaining, w.UsedPercent)
		if w.NextResetMS > 0 {
			fmt.Fprintf(&b, ", resets %s", resetIn(w.NextResetMS))
		}
		b.WriteString(")\n")
	}
	return b.String()
}

func windowLabel(kind string) string {
	switch kind {
	case "5h":
		return "5h"
	case "weekly":
		return "weekly"
	case "mcp":
		return "MCP"
	}
	return kind
}

// resetIn renders the time until an epoch-ms reset timestamp.
func resetIn(ms int64) string {
	d := time.Until(time.UnixMilli(ms)).Round(time.Minute)
	if d <= 0 {
		return "soon"
	}
	return "in " + d.String()
}

// parseResetMS accepts the reset-timestamp spellings providers actually use:
// RFC3339 strings, epoch seconds, or epoch milliseconds.
func parseResetMS(v any) int64 {
	switch t := v.(type) {
	case string:
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05Z0700", "2006-01-02 15:04:05"} {
			if ts, err := time.Parse(layout, t); err == nil {
				return ts.UnixMilli()
			}
		}
	case float64:
		ms := int64(t)
		switch {
		case ms <= 0:
			return 0
		case ms < 1_000_000_000_000: // epoch seconds
			return ms * 1000
		default:
			return ms
		}
	}
	return 0
}

// asPercent accepts a fraction (0-1) or a percentage (0-100).
func asPercent(v any) float64 {
	f, ok := v.(float64)
	if !ok {
		return 0
	}
	if f <= 1 {
		return f * 100
	}
	return f
}
