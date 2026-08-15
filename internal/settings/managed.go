package settings

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
)

// Managed settings (a port of Claude Code's enterprise managed-settings
// tier): a settings file that overrides user/database values AND locks every
// field it sets against further changes (/settings and Save reject writes to
// locked fields). Sourced from --managed-settings / WOCA_MANAGED_SETTINGS.

// managedFile is the on-disk shape: the persisted Settings fields.
type managedFile struct {
	Settings
	// Locked optionally lists extra keys to lock without setting them.
	Locked []string `json:"locked,omitempty"`
}

// LoadManaged reads a managed-settings file.
func LoadManaged(path string) (Settings, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Settings{}, fmt.Errorf("settings: managed file: %w", err)
	}
	var mf managedFile
	if err := json.Unmarshal(data, &mf); err != nil {
		return Settings{}, fmt.Errorf("settings: parse managed file %s: %w", path, err)
	}
	return mf.Settings, nil
}

// managedState overlays a Store: values that override DB reads and lock
// writes.
type managedState struct {
	mu      sync.Mutex
	overlay *Settings
}

// applyOverlay merges the overlay onto db-loaded settings (overlay wins).
func (m *managedState) applyOverlay(out *Settings) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.overlay == nil {
		return
	}
	o := m.overlay
	if o.Provider != "" {
		out.Provider = o.Provider
	}
	if o.Model != "" {
		out.Model = o.Model
	}
	if o.MaxTokens != 0 {
		out.MaxTokens = o.MaxTokens
	}
	if o.Temperature != 0 {
		out.Temperature = o.Temperature
	}
	if o.AnthropicBaseURL != "" {
		out.AnthropicBaseURL = o.AnthropicBaseURL
	}
	if o.OpenAIBaseURL != "" {
		out.OpenAIBaseURL = o.OpenAIBaseURL
	}
	if o.CheckpointBackend != "" {
		out.CheckpointBackend = o.CheckpointBackend
	}
	if o.MemoryBackend != "" {
		out.MemoryBackend = o.MemoryBackend
	}
	if o.CheckpointDir != "" {
		out.CheckpointDir = o.CheckpointDir
	}
	if o.OutputStyle != "" {
		out.OutputStyle = o.OutputStyle
	}
	if o.MemoryEngine != "" {
		out.MemoryEngine = o.MemoryEngine
	}
	if o.LadyMDBPath != "" {
		out.LadyMDBPath = o.LadyMDBPath
	}
	if o.LadyMEmbedding != "" {
		out.LadyMEmbedding = o.LadyMEmbedding
	}
	if o.LadyMEmbeddingModel != "" {
		out.LadyMEmbeddingModel = o.LadyMEmbeddingModel
	}
	if o.LadyMEmbeddingBaseURL != "" {
		out.LadyMEmbeddingBaseURL = o.LadyMEmbeddingBaseURL
	}
	if o.LadyMLLM != "" {
		out.LadyMLLM = o.LadyMLLM
	}
	if o.LadyMTopK != 0 {
		out.LadyMTopK = o.LadyMTopK
	}
	if o.LadyMSystem2Interval != 0 {
		out.LadyMSystem2Interval = o.LadyMSystem2Interval
	}
	// Bool fields (LadyMAutoRetain, LadyMSystem2 — like DebugLog) cannot be
	// overlay-managed: false == unset, so a managed "false" is indistinguishable
	// from an unmanaged field and therefore cannot be locked either.
}

// checkWritable rejects saves that would change a locked field.
func (m *managedState) checkWritable(cfg Settings) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.overlay == nil {
		return nil
	}
	o := m.overlay
	checks := []struct {
		key     string
		managed string
		desired string
	}{
		{"provider", o.Provider, cfg.Provider},
		{"model", o.Model, cfg.Model},
		{"anthropic_base_url", o.AnthropicBaseURL, cfg.AnthropicBaseURL},
		{"openai_base_url", o.OpenAIBaseURL, cfg.OpenAIBaseURL},
		{"checkpoint_backend", o.CheckpointBackend, cfg.CheckpointBackend},
		{"memory_backend", o.MemoryBackend, cfg.MemoryBackend},
		{"checkpoint_dir", o.CheckpointDir, cfg.CheckpointDir},
		{"output_style", o.OutputStyle, cfg.OutputStyle},
		{"memory_engine", o.MemoryEngine, cfg.MemoryEngine},
		{"ladym_db_path", o.LadyMDBPath, cfg.LadyMDBPath},
		{"ladym_embedding", o.LadyMEmbedding, cfg.LadyMEmbedding},
		{"ladym_embedding_model", o.LadyMEmbeddingModel, cfg.LadyMEmbeddingModel},
		{"ladym_embedding_base_url", o.LadyMEmbeddingBaseURL, cfg.LadyMEmbeddingBaseURL},
		{"ladym_llm", o.LadyMLLM, cfg.LadyMLLM},
	}
	for _, c := range checks {
		if c.managed == "" {
			continue // not managed
		}
		if c.desired != "" && c.desired != c.managed {
			return fmt.Errorf("settings: %s is managed and locked (set to %s)", c.key, c.managed)
		}
	}
	if o.MaxTokens != 0 && cfg.MaxTokens != 0 && cfg.MaxTokens != o.MaxTokens {
		return fmt.Errorf("settings: max_tokens is managed and locked (set to %d)", o.MaxTokens)
	}
	if o.Temperature != 0 && cfg.Temperature != 0 && cfg.Temperature != o.Temperature {
		return fmt.Errorf("settings: temperature is managed and locked (set to %v)", o.Temperature)
	}
	if o.LadyMTopK != 0 && cfg.LadyMTopK != 0 && cfg.LadyMTopK != o.LadyMTopK {
		return fmt.Errorf("settings: ladym_topk is managed and locked (set to %d)", o.LadyMTopK)
	}
	if o.LadyMSystem2Interval != 0 && cfg.LadyMSystem2Interval != 0 && cfg.LadyMSystem2Interval != o.LadyMSystem2Interval {
		return fmt.Errorf("settings: ladym_system2_interval is managed and locked (set to %d)", o.LadyMSystem2Interval)
	}
	// LadyMAutoRetain and LadyMSystem2 (bools, like DebugLog) are not
	// lockable here: a managed false is indistinguishable from unset.
	return nil
}
