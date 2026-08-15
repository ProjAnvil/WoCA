package memory

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ProjAnvil/LadyM/adapter"
	"github.com/ProjAnvil/LadyM/config"
	ladymengine "github.com/ProjAnvil/LadyM/engine"
	ladymfacade "github.com/ProjAnvil/LadyM/ladym"
	ladymops "github.com/ProjAnvil/LadyM/operations"
	"github.com/ProjAnvil/LadyM/schema"

	"github.com/projanvil/langchain-golang/core/language"
)

// embedKeyEnv is the environment variable ladyM's built-in openai embedding
// provider reads its API key from. woca owns the process, so Build sets it
// from its own resolved secrets instead of ladyM's secret store.
const embedKeyEnv = "WOCA_LADYM_EMBED_KEY"

// BuildInput carries everything Build needs, resolved by the caller (main).
type BuildInput struct {
	EngineEnabled    bool
	DBPath           string // absolute; "~" already expanded
	DefaultWorkspace string
	Embedding        string // inherit | hashing | openai | ollama | http
	EmbeddingModel   string
	EmbeddingBaseURL string
	EmbeddingAPIKey  string
	LLM              string             // inherit | none
	ChatModel        language.ChatModel // woca's active model (LLM=inherit)
	OpenAIKey        string
	OpenAIBaseURL    string
}

// DefaultDBPath returns ~/.woca/ladym.db (empty string on home-dir failure).
func DefaultDBPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".woca", "ladym.db")
}

// ExpandHome resolves a leading ~ in a user-supplied db path.
func ExpandHome(p string) string {
	if p == "~" || len(p) >= 2 && p[0] == '~' && p[1] == filepath.Separator {
		home, err := os.UserHomeDir()
		if err == nil {
			if p == "~" {
				return home
			}
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// resolveEmbedding maps settings values to a concrete ladyM embedding
// provider config: inherit falls back to woca's OpenAI-compatible setup when
// present, else to the offline hashing provider.
func resolveEmbedding(in BuildInput) (provider, model, baseURL, key string) {
	switch in.Embedding {
	case "hashing", "ollama", "http":
		return in.Embedding, in.EmbeddingModel, in.EmbeddingBaseURL, in.EmbeddingAPIKey
	case "openai":
		return "openai",
			orDefault(in.EmbeddingModel, "text-embedding-3-small"),
			orDefault(in.EmbeddingBaseURL, in.OpenAIBaseURL),
			orDefault(in.EmbeddingAPIKey, in.OpenAIKey)
	default: // inherit
		if in.OpenAIKey != "" || in.OpenAIBaseURL != "" || in.EmbeddingAPIKey != "" {
			return "openai",
				orDefault(in.EmbeddingModel, "text-embedding-3-small"),
				orDefault(in.EmbeddingBaseURL, in.OpenAIBaseURL),
				orDefault(in.EmbeddingAPIKey, in.OpenAIKey)
		}
		return "hashing", "", "", ""
	}
}

// Build constructs the in-process ladyM engine. The config is assembled in
// code (no TOML / LADYM_* env chain) so woca stays the single source of
// truth for db path, workspace, and providers.
func Build(in BuildInput) (*LadyM, error) {
	if !in.EngineEnabled {
		return nil, fmt.Errorf("memory: engine disabled")
	}
	if in.DBPath == "" {
		return nil, fmt.Errorf("memory: db path is required")
	}
	provider, model, baseURL, key := resolveEmbedding(in)

	cfg := config.Default()
	cfg.DBPath = in.DBPath
	cfg.Workspace = in.DefaultWorkspace
	cfg.EnableWAL = true
	// woca-managed re-embedding: switching embedding providers rebuilds the
	// vector index in place at engine open.
	cfg.EmbeddingAllowDimChange = true
	cfg.EmbeddingProvider = provider
	cfg.EmbeddingModel = model
	cfg.EmbeddingBaseURL = baseURL
	cfg.EmbeddingAPIKeyEnv = embedKeyEnv
	if key != "" {
		os.Setenv(embedKeyEnv, key)
	}
	// The LLM is injected per-op below (host model), never resolved by
	// ladyM's own provider factories.
	cfg.LLMProvider = "none"

	var (
		eng *ladymengine.Engine
		err error
	)
	if in.LLM == "inherit" && in.ChatModel != nil {
		eng, err = ladymfacade.NewEngineWithModels(cfg, &adapter.ModelRouting{
			Consolidate: adapter.WrapChatModel(in.ChatModel, "function_calling"),
		})
	} else {
		eng, err = ladymfacade.NewEngine(cfg)
	}
	if err != nil {
		return nil, fmt.Errorf("memory: build ladyM engine: %w", err)
	}
	return &LadyM{eng: eng, path: in.DBPath}, nil
}

// LadyM implements Engine over an in-process ladyM engine.
type LadyM struct {
	eng  *ladymengine.Engine
	path string
}

var _ Engine = (*LadyM)(nil)

func (l *LadyM) workspace(ws string) string {
	if ws == "" {
		return l.eng.Config.Workspace
	}
	return ws
}

// Recall runs the two-tier semantic retrieval (heuristic, no LLM).
func (l *LadyM) Recall(_ context.Context, query, workspace string, topK int) ([]Hit, error) {
	if topK <= 0 {
		topK = 6
	}
	resp, err := l.eng.Recall(query, l.workspace(workspace), topK, nil, nil, 0)
	if err != nil {
		return nil, fmt.Errorf("memory: recall: %w", err)
	}
	return hitsFromRecall(resp), nil
}

// SearchCode searches indexed code symbols.
func (l *LadyM) SearchCode(_ context.Context, query, workspace string, topK int) ([]Hit, error) {
	if topK <= 0 {
		topK = 8
	}
	resp, err := l.eng.SearchCode(query, topK, l.workspace(workspace))
	if err != nil {
		return nil, fmt.Errorf("memory: search code: %w", err)
	}
	return hitsFromRecall(resp), nil
}

// Remember stores a semantic fact (subject to ladyM's attention gate).
func (l *LadyM) Remember(_ context.Context, workspace, content string, tags []string, source string) (string, error) {
	ws := l.workspace(workspace)
	m, err := l.eng.Remember(content, schema.LayerSemantic, schema.TypeFact, tags, map[string]any{"workspace": ws}, source, "")
	if err != nil {
		return "", fmt.Errorf("memory: remember: %w", err)
	}
	if m == nil {
		return "", fmt.Errorf("memory: remember returned no memory")
	}
	return m.ID, nil
}

// RecordEvent stores an L1 episodic tuple (food for consolidation).
func (l *LadyM) RecordEvent(_ context.Context, workspace, agentName, action, observation, outcome string) error {
	_, err := l.eng.RecordEvent(agentName, action, observation, outcome, nil, nil)
	if err != nil {
		return fmt.Errorf("memory: record event: %w", err)
	}
	return nil
}

// List returns recent memories newest-first (layer may be "" for all).
func (l *LadyM) List(_ context.Context, workspace, layer string, limit, offset int) ([]Hit, error) {
	if limit <= 0 {
		limit = 20
	}
	var layerPtr *schema.Layer
	if layer != "" {
		l := schema.Layer(layer)
		layerPtr = &l
	}
	items, err := l.eng.List(l.workspace(workspace), layerPtr, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("memory: list: %w", err)
	}
	out := make([]Hit, 0, len(items))
	for _, m := range items {
		if m == nil {
			continue
		}
		out = append(out, hitFromMemory(m, 0))
	}
	return out, nil
}

// Forget deletes one memory by id.
func (l *LadyM) Forget(_ context.Context, id string) error {
	if err := l.eng.Forget(id); err != nil {
		return fmt.Errorf("memory: forget: %w", err)
	}
	return nil
}

// Stats counts memories per layer for one workspace.
func (l *LadyM) Stats(_ context.Context, workspace string) (*Stats, error) {
	st, err := l.eng.StatsFor(l.workspace(workspace))
	if err != nil {
		return nil, fmt.Errorf("memory: stats: %w", err)
	}
	return &Stats{Total: st.TotalMemories, Layers: st.ByLayer}, nil
}

// IndexCode (re)indexes a source root into code memories; returns a report.
func (l *LadyM) IndexCode(_ context.Context, root, workspace string, force bool) (string, error) {
	rep, err := l.eng.IndexCode(root, force, l.workspace(workspace), nil)
	if err != nil {
		return "", fmt.Errorf("memory: index code: %w", err)
	}
	return fmt.Sprintf("index report: %+v", rep), nil
}

// Consolidate runs one System 2 cycle synchronously; returns a report.
func (l *LadyM) Consolidate(_ context.Context, workspace string) (string, error) {
	rep, err := ladymops.RunSystem2Cycle(l.eng, l.workspace(workspace))
	if err != nil {
		return "", fmt.Errorf("memory: consolidate: %w", err)
	}
	return fmt.Sprintf("consolidate=%+v proceduralize=%+v skipped_llm_steps=%v decay=%+v",
		rep.Consolidate, rep.Proceduralize, rep.SkippedLLMSteps, rep.Decay), nil
}

// Description names the engine for /memory stats output.
func (l *LadyM) Description() string {
	return "ladym " + l.path
}

// Close releases the underlying store.
func (l *LadyM) Close() error {
	if err := l.eng.Close(); err != nil {
		return fmt.Errorf("memory: close: %w", err)
	}
	return nil
}

func hitFromMemory(m *schema.Memory, score float64) Hit {
	summary := m.Summary
	if summary == "" {
		summary = m.Content
	}
	return Hit{
		ID: m.ID, Layer: string(m.Layer), Type: string(m.Type),
		Summary: summary, Content: m.Content, Source: m.Source,
		Tags: m.Tags, Score: score, UpdatedAt: m.UpdatedAt,
	}
}

func hitsFromRecall(resp *schema.RecallResponse) []Hit {
	out := make([]Hit, 0, len(resp.Results))
	for _, r := range resp.Results {
		if r == nil || r.Memory == nil {
			continue
		}
		out = append(out, hitFromMemory(r.Memory, r.Score))
	}
	return out
}
