// Package memory adapts the ladyM memory engine into woca. The Engine
// interface is the narrow surface consumed by the agent loop, the console,
// and the web API; ladyM is the only implementation, and a nil Engine means
// the memory subsystem is disabled.
package memory

import "context"

// Hit is one memory item on the read path (recall, search, list).
type Hit struct {
	ID        string   `json:"id"`
	Layer     string   `json:"layer"`
	Type      string   `json:"type"`
	Summary   string   `json:"summary"`
	Content   string   `json:"content"`
	Source    string   `json:"source"`
	Tags      []string `json:"tags"`
	Score     float64  `json:"score,omitempty"`
	UpdatedAt float64  `json:"updated_at"`
}

// Stats summarizes one workspace's memory.
type Stats struct {
	Total  int            `json:"total"`
	Layers map[string]int `json:"layers"`
}

// Engine is the factual-memory subsystem. All methods take a workspace
// (row-level isolation inside one sqlite db). Implementations must be safe
// for concurrent use.
type Engine interface {
	// Recall runs the two-tier semantic retrieval (heuristic, no LLM).
	Recall(ctx context.Context, query, workspace string, topK int) ([]Hit, error)
	// SearchCode searches indexed code symbols.
	SearchCode(ctx context.Context, query, workspace string, topK int) ([]Hit, error)
	// Remember stores a semantic fact (subject to ladyM's attention gate).
	Remember(ctx context.Context, workspace, content string, tags []string, source string) (string, error)
	// RecordEvent stores an L1 episodic tuple (food for consolidation).
	RecordEvent(ctx context.Context, workspace, agentName, action, observation, outcome string) error
	// List returns recent memories newest-first (layer may be "" for all).
	List(ctx context.Context, workspace, layer string, limit, offset int) ([]Hit, error)
	// Forget deletes one memory by id.
	Forget(ctx context.Context, id string) error
	// Stats counts memories per layer for one workspace.
	Stats(ctx context.Context, workspace string) (*Stats, error)
	// IndexCode (re)indexes a source root into code memories; returns a report.
	IndexCode(ctx context.Context, root, workspace string, force bool) (string, error)
	// Consolidate runs one System 2 cycle synchronously; returns a report.
	Consolidate(ctx context.Context, workspace string) (string, error)
	// Description names the engine for /memory stats output.
	Description() string
	// Close releases the underlying store.
	Close() error
}
