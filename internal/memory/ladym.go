package memory

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sync"

	"github.com/ProjAnvil/LadyM/adapter"
	"github.com/ProjAnvil/LadyM/config"
	ladymengine "github.com/ProjAnvil/LadyM/engine"
	ladymfacade "github.com/ProjAnvil/LadyM/ladym"
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

// officialOpenAI reports whether a base URL points at the official OpenAI
// API (the only inherited endpoint we can assume serves /embeddings —
// chat-completions-only OpenAI-compatible servers like deepseek do not).
func officialOpenAI(baseURL string) bool {
	u, err := url.Parse(orDefault(baseURL, "https://api.openai.com/v1"))
	if err != nil {
		return false
	}
	return u.Host == "api.openai.com"
}

// resolveEmbedding maps settings values to a concrete ladyM embedding
// provider config. inherit chooses the OpenAI embeddings API only when the
// inherited endpoint is trustworthy: an explicit embedding key, or woca's
// OpenAI config pointing at the official api.openai.com. Chat-only
// OpenAI-compatible providers (deepseek etc.) fall back to the offline
// hashing provider instead of breaking engine startup with a 404 probe.
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
		if in.EmbeddingAPIKey != "" ||
			(in.OpenAIKey != "" && officialOpenAI(in.OpenAIBaseURL)) {
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
	// SetWorkspace("") is a no-op in ladyM, so an empty default would leave
	// the engine parked on whatever workspace the last write retargeted it
	// to. Refuse to build in that state.
	if in.DefaultWorkspace == "" {
		return nil, fmt.Errorf("memory: default workspace is required")
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
		_ = os.Setenv(embedKeyEnv, key)
	} else {
		// A rebuild without a key must not inherit a stale key left in the
		// process env by an earlier Build.
		_ = os.Unsetenv(embedKeyEnv)
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
	return &LadyM{eng: eng, path: in.DBPath, defWs: in.DefaultWorkspace}, nil
}

// LadyM implements Engine over an in-process ladyM engine.
//
// ladyM v0.6.0 made the engine safe for concurrent front-ends: layer state
// lives behind the storage backend (SQLite WAL single-writer, Postgres
// parallel), the engine mutex guards only lazy LLM-agent wiring, and
// Engine.Scope(ws) hands out workspace-bound handles so writes never
// retarget shared state. This wrapper therefore takes no lock on operation
// paths and routes every write through a Scope; mu serializes Close against
// itself only — Close is a teardown-time call, not interleaved with ops.
type LadyM struct {
	mu    sync.Mutex
	eng   *ladymengine.Engine
	path  string
	defWs string
}

var _ Engine = (*LadyM)(nil)

// workspace resolves an optional per-call workspace to the engine default.
func (l *LadyM) workspace(ws string) string {
	if ws == "" {
		return l.defWs
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
// It returns an empty id with a nil error when the gate drops the content:
// ladyM hands back an unpersisted memory marked Metadata["gated"]=="dropped",
// which this adapter never reports as stored.
func (l *LadyM) Remember(_ context.Context, workspace, content string, tags []string, source string) (string, error) {
	scope := l.eng.Scope(l.workspace(workspace))
	m, err := scope.Remember(content, schema.LayerSemantic, schema.TypeFact, tags, nil, source, "")
	if err != nil {
		return "", fmt.Errorf("memory: remember: %w", err)
	}
	if m == nil {
		return "", fmt.Errorf("memory: remember returned no memory")
	}
	if m.Metadata["gated"] == "dropped" {
		return "", nil // dropped by the attention gate; nothing was persisted
	}
	return m.ID, nil
}

// RecordEvent stores an L1 episodic tuple (food for consolidation).
func (l *LadyM) RecordEvent(_ context.Context, workspace, agentName, action, observation, outcome string) error {
	scope := l.eng.Scope(l.workspace(workspace))
	if _, err := scope.RecordEvent(agentName, action, observation, outcome, nil, nil); err != nil {
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
		ly := schema.Layer(layer)
		layerPtr = &ly
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
	return fmt.Sprintf("indexed %d of %d files (%d symbols, %d refs, %d unchanged, %d errors)",
		rep.FilesIndexed, rep.FilesSeen, rep.SymbolsWritten, rep.RefsWritten,
		rep.FilesSkippedUnchanged, len(rep.Errors)), nil
}

// Consolidate runs one System 2 cycle: episodes created at or after since
// (0 = full backfill) are promoted into facts through the ADD/UPDATE/DELETE
// classifier, playbooks cluster, the L5/L6 extractions run once enough
// episodes exist, and stale episodes decay. The cycle is composed here
// instead of via ladymops.RunSystem2Cycle because woca drives incremental
// cycles (since = the previous trigger) and that helper hardcodes a full
// backfill — with an inherited classifier that would re-bill every stored
// episode on every cycle.
func (l *LadyM) Consolidate(_ context.Context, workspace string, since float64) (string, error) {
	ws := l.workspace(workspace)
	cons, err := l.eng.Consolidate(ws, since)
	if err != nil {
		return "", fmt.Errorf("memory: consolidate: %w", err)
	}
	if _, err := l.eng.Proceduralize(ws, 0); err != nil {
		return "", fmt.Errorf("memory: consolidate: %w", err)
	}
	skipped := true
	n, err := l.eng.CountRecentEpisodes(ws)
	if err != nil {
		return "", fmt.Errorf("memory: consolidate: %w", err)
	}
	if n >= l.eng.MinEpisodesToRun() {
		skipped = false
		if _, err := l.eng.ExtractMentalModels(ws); err != nil {
			return "", fmt.Errorf("memory: consolidate: %w", err)
		}
		if _, err := l.eng.PredictForwardIntents(ws); err != nil {
			return "", fmt.Errorf("memory: consolidate: %w", err)
		}
	}
	decay, err := l.eng.Decay(ws, false, 0, 0)
	if err != nil {
		return "", fmt.Errorf("memory: consolidate: %w", err)
	}
	// L4 self-maintenance: drop edges whose endpoint memories are gone
	// (legacy pre-FK databases accumulate them; the sweep is a no-op on a
	// healthy graph).
	swept, err := l.eng.MaintainL4(ws)
	if err != nil {
		return "", fmt.Errorf("memory: consolidate: %w", err)
	}
	return fmt.Sprintf("facts_promoted=%d kept_episodes=%d skipped_llm_steps=%v decay=%+v edges_swept=%d",
		cons.PromotedToSemantic, cons.KeptEpisodes, skipped, decay, swept.Swept), nil
}

// Description names the engine for /memory stats output.
func (l *LadyM) Description() string {
	return "ladym " + l.path
}

// Close releases the underlying store.
func (l *LadyM) Close() error {
	l.mu.Lock()
	err := l.eng.Close()
	l.mu.Unlock()
	if err != nil {
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
