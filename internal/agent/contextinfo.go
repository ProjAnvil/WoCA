package agent

// Context-usage introspection: the payload behind GET
// /api/sessions/{id}/context. The breakdown estimates what the next model
// request would carry (mirroring the categories Claude Code's /context
// reports), while LastRequest carries the provider-reported usage of the
// most recent call — the exact counterpart of the estimate.

import (
	"context"
	"encoding/json"

	"github.com/projanvil/langchain-golang/core/messages"
	coretools "github.com/projanvil/langchain-golang/core/tools"

	"github.com/projanvil/woca/internal/prompt"
	"github.com/projanvil/woca/internal/runws"
	"github.com/projanvil/woca/internal/tokens"
)

// ContextCategory is one line of the context breakdown (all token counts are
// heuristic estimates; only LastRequest is provider-reported).
type ContextCategory struct {
	Key    string `json:"key"`
	Label  string `json:"label"`
	Tokens int    `json:"tokens"`
	Count  int    `json:"count,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// ContextInfo is the context-usage snapshot for one session.
type ContextInfo struct {
	Model            string            `json:"model"`
	Categories       []ContextCategory `json:"categories"`
	EstimatedTotal   int               `json:"estimated_total"`
	LastRequest      *LastUsage        `json:"last_request,omitempty"`
	CompactThreshold int               `json:"compact_threshold,omitempty"`
	// ContextWindow is the model's context-window limit (0 = unknown). It is
	// filled by the server layer, which knows the provider.
	ContextWindow int `json:"context_window,omitempty"`
}

// ContextBreakdown estimates the composition of the next model request for
// the given history: system prompt (with the memory-files share called out),
// tool schemas (built-in vs MCP/extra), and the conversation history by
// role. It also carries the most recent call's provider-reported usage and
// the auto-compact threshold. Pure in-memory computation apart from reading
// the workspace's memory files (a handful of small instruction files).
func (a *Agent) ContextBreakdown(ctx context.Context, history []messages.Message) ContextInfo {
	ws := a.workdir
	if v, ok := runws.From(ctx); ok && v != "" {
		ws = v
	}

	// System prompt: the built prompt minus the memory files is the
	// "base" share (environment sections, git status, skills listing).
	// Memory files are only part of the prompt when a builder is wired
	// (harness builds them in); without one the startup prompt is all there
	// is, so no memory category is reported.
	sysTokens := tokens.EstimateTextWith(a.model, a.systemPromptFor(ctx))
	var mem prompt.MemoryFileStats
	if a.promptFor != nil {
		mem = prompt.MemoryFileStatsFor(ws)
	}

	// Tool schemas: what BindTools serializes per tool (name + description +
	// JSON schema), split into built-in vs MCP/extra.
	a.mu.Lock()
	base := append([]coretools.Tool(nil), a.baseTools...)
	extra := append([]coretools.Tool(nil), a.extraTools...)
	a.mu.Unlock()
	baseToolTokens, _ := estimateToolSet(base)
	extraToolTokens, extraN := estimateToolSet(extra)

	// History by role. Auto-compact inserts its summary as a System message
	// mid-history (compact.go), so that role gets its own category — leaving
	// it out would under-report after every compaction.
	var humanMsgs, aiMsgs, toolMsgs, sysMsgs []messages.Message
	for _, m := range history {
		switch m.Role {
		case messages.RoleHuman:
			humanMsgs = append(humanMsgs, m)
		case messages.RoleAI:
			aiMsgs = append(aiMsgs, m)
		case messages.RoleTool:
			toolMsgs = append(toolMsgs, m)
		case messages.RoleSystem:
			sysMsgs = append(sysMsgs, m)
		}
	}

	sysBase := sysTokens - mem.TotalTokens
	if sysBase < 0 {
		sysBase = 0
	}
	cats := []ContextCategory{
		{Key: "system", Label: "系统提示词", Tokens: sysBase, Detail: "基础段落 + git 状态 + skills 清单"},
		{Key: "memory", Label: "记忆文件", Tokens: mem.TotalTokens, Count: mem.Files, Detail: "CLAUDE.md / AGENTS.md"},
		{Key: "tools", Label: "工具 schema", Tokens: baseToolTokens, Count: len(base), Detail: "内置工具"},
	}
	if extraN > 0 {
		cats = append(cats, ContextCategory{
			Key: "tools_extra", Label: "工具 schema", Tokens: extraToolTokens, Count: extraN,
			Detail: "MCP / 扩展工具",
		})
	}
	humanTok := tokens.EstimateWith(a.model, humanMsgs)
	aiTok := tokens.EstimateWith(a.model, aiMsgs)
	toolTok := tokens.EstimateWith(a.model, toolMsgs)
	if len(sysMsgs) > 0 {
		cats = append(cats, ContextCategory{
			Key: "history_summary", Label: "压缩摘要", Tokens: tokens.Estimate(sysMsgs),
			Count: len(sysMsgs), Detail: "auto-compact 生成的早期对话摘要",
		})
	}
	cats = append(cats,
		ContextCategory{Key: "history_human", Label: "对话历史", Tokens: humanTok, Count: len(humanMsgs), Detail: "用户消息"},
		ContextCategory{Key: "history_ai", Label: "对话历史", Tokens: aiTok, Count: len(aiMsgs), Detail: "助手消息"},
		ContextCategory{Key: "history_tool", Label: "对话历史", Tokens: toolTok, Count: len(toolMsgs), Detail: "工具调用结果"},
	)

	info := ContextInfo{
		Model:            a.CurrentModelName(),
		Categories:       cats,
		CompactThreshold: a.compactLimit,
	}
	for _, c := range cats {
		info.EstimatedTotal += c.Tokens
	}
	if lu := a.LastUsage(); lu.InputTokens+lu.CacheReadTokens+lu.CacheWriteTokens > 0 {
		info.LastRequest = &lu
	}
	return info
}

// estimateToolSet approximates the serialized size of one tool set: the
// JSON-schema payloads BindTools sends per tool.
func estimateToolSet(ts []coretools.Tool) (tok int, count int) {
	for _, t := range ts {
		count++
		tok += tokens.EstimateText(t.Name()) + tokens.EstimateText(t.Description())
		if raw, err := json.Marshal(t.ArgsSchema()); err == nil {
			tok += tokens.EstimateText(string(raw))
		}
	}
	return tok, count
}
