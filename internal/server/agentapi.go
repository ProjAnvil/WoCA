package server

// Runtime agent controls for the chat UI: the permission mode and thinking
// level selectors next to the chat input, plus the provider-quota chip.
// GET /api/agent reports the live state; the POST endpoints switch it; GET
// /api/quota returns the provider quota (GLM Coding Plan) with the session
// usage as an always-available fallback.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/cost"
	"github.com/projanvil/woca/internal/quota"
)

// quotaTimeout bounds the upstream monitor request.
const quotaTimeout = 10 * time.Second

// agentState is the GET /api/agent payload.
type agentState struct {
	Provider       string `json:"provider"`
	Model          string `json:"model"`
	PermissionMode string `json:"permission_mode"`
	PlanMode       bool   `json:"plan_mode"`
	Thinking       struct {
		Level  string `json:"level"`
		Budget int    `json:"budget"`
		// Supported is false when the current provider's model has no
		// thinking mode — the UI hides/disables the thinking selector then.
		Supported bool     `json:"supported"`
		Levels    []string `json:"levels"`
		Note      string   `json:"note,omitempty"`
	} `json:"thinking"`
}

// liveConfig returns the agent's effective configuration. A nil CfgLive
// yields zero values (tests, embedders).
func (s *Server) liveConfig() config.Config {
	if s.cfgLive == nil {
		return config.Config{}
	}
	return s.cfgLive()
}

func (s *Server) handleGetAgent(w http.ResponseWriter, _ *http.Request) {
	if s.agent == nil {
		writeError(w, http.StatusServiceUnavailable, "agent is not available")
		return
	}
	cfg := s.liveConfig()
	var out agentState
	out.Provider = string(cfg.Provider)
	out.Model = cfg.Model
	out.PermissionMode = string(s.agent.Mode())
	out.PlanMode = s.agent.PlanMode()
	out.Thinking.Budget = cfg.ThinkingBudget
	out.Thinking.Level = config.ThinkingLevel(cfg.ThinkingBudget)
	out.Thinking.Levels = config.ThinkingLevelsFor(cfg.Provider, cfg.Model)
	out.Thinking.Supported = len(out.Thinking.Levels) > 0
	out.Thinking.Note = config.ThinkingSupportNote(cfg.Provider)
	writeJSON(w, http.StatusOK, out)
}

// handleSetPermissionMode switches the permission gate mode (default |
// acceptEdits | bypassPermissions | readOnly | plan) for subsequent tool
// calls; it takes effect immediately, even mid-run.
func (s *Server) handleSetPermissionMode(w http.ResponseWriter, r *http.Request) {
	if s.agent == nil {
		writeError(w, http.StatusServiceUnavailable, "agent is not available")
		return
	}
	var req struct {
		Mode string `json:"mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	mode := config.PermissionMode(req.Mode)
	switch mode {
	case config.ModeDefault, config.ModeAcceptEdits, config.ModeBypass, config.ModeReadOnly, config.ModePlan:
	default:
		writeError(w, http.StatusBadRequest, "unknown permission mode "+req.Mode)
		return
	}
	s.agent.SetMode(mode)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "mode": mode})
}

// handleSetThinking switches the extended-thinking level (off | low |
// medium | high, or an explicit token budget). The change persists to the
// settings database and rebuilds the chat model; it applies from the next
// model call on.
func (s *Server) handleSetThinking(w http.ResponseWriter, r *http.Request) {
	if s.agent == nil {
		writeError(w, http.StatusServiceUnavailable, "agent is not available")
		return
	}
	if s.setThinking == nil {
		writeError(w, http.StatusServiceUnavailable, "thinking switching is not available in this session")
		return
	}
	var req struct {
		Level  string `json:"level"`
		Budget *int   `json:"budget"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	var budget int
	switch {
	case req.Budget != nil:
		budget = *req.Budget
	case req.Level != "":
		b, ok := config.ThinkingBudgetFor(req.Level)
		if !ok {
			writeError(w, http.StatusBadRequest, "unknown thinking level "+req.Level+" (off | low | medium | high)")
			return
		}
		budget = b
	default:
		writeError(w, http.StatusBadRequest, "level or budget is required")
		return
	}
	if budget != 0 && (budget < config.MinThinkingBudget || budget > config.MaxThinkingBudget) {
		writeError(w, http.StatusBadRequest, "thinking budget must be 0 or within [1024, 31999]")
		return
	}
	// The provider template decides whether the current model can think at
	// all (Anthropic 3.7+/4.x budget, OpenAI o-series/gpt-5 reasoning_effort,
	// GLM 4.5+ thinking, DeepSeek's thinking switch).
	cfg := s.liveConfig()
	levels := config.ThinkingLevelsFor(cfg.Provider, cfg.Model)
	if budget > 0 && !containsLevel(levels, config.ThinkingLevel(budget)) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("model %s (%s) does not support thinking (levels %v)", cfg.Model, cfg.Provider, levels))
		return
	}
	if err := s.setThinking(budget); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"level":  config.ThinkingLevel(budget),
		"budget": budget,
	})
}

func containsLevel(levels []string, level string) bool {
	for _, l := range levels {
		if l == level {
			return true
		}
	}
	return false
}

// handleQuota reports the provider quota plus this process's session usage
// (the always-available fallback when the provider has no quota API).
func (s *Server) handleQuota(w http.ResponseWriter, r *http.Request) {
	out := struct {
		Quota   quota.Info     `json:"quota"`
		Session map[string]any `json:"session"`
	}{Quota: quota.Info{Available: false, Reason: "该部署未配置额度查询"}}

	if s.quotaFetch != nil {
		ctx, cancel := context.WithTimeout(r.Context(), quotaTimeout)
		info, err := s.quotaFetch(ctx)
		cancel()
		if err != nil {
			out.Quota = quota.Info{Available: false, Reason: err.Error()}
		} else {
			out.Quota = info
		}
	}

	if s.agent != nil {
		u := s.agent.Usage()
		model := s.liveConfig().Model
		c, known := cost.Estimate(model, cost.Usage{
			Requests:         u.Requests,
			InputTokens:      u.InputTokens,
			OutputTokens:     u.OutputTokens,
			CacheReadTokens:  u.CacheReadTokens,
			CacheWriteTokens: u.CacheWriteTokens,
		})
		out.Session = map[string]any{
			"requests":           u.Requests,
			"input_tokens":       u.InputTokens,
			"output_tokens":      u.OutputTokens,
			"cache_read_tokens":  u.CacheReadTokens,
			"cache_write_tokens": u.CacheWriteTokens,
			"cost_usd":           c,
			"cost_known":         known,
			"model":              model,
		}
	} else {
		out.Session = map[string]any{}
	}
	writeJSON(w, http.StatusOK, out)
}
