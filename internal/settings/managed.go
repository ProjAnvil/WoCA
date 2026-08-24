package settings

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"sync"
)

// ErrManagedLocked rejects a save that would change an overlay-locked
// field; the settings API maps it to 403 by identity, never by message.
var ErrManagedLocked = errors.New("settings: managed and locked")

// Managed settings (an enterprise tier): a settings file that overrides
// user/database values AND locks every field it sets against further changes
// (/settings and Save reject writes to locked fields). Sourced from
// --managed-settings / WOCA_MANAGED_SETTINGS.

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
	if o.ModelWhitelist != "" {
		out.ModelWhitelist = o.ModelWhitelist
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
	// Private-cloud storage/memory wiring (product §3 storage domain): the
	// config plane delivers the per-subsystem postgres coordinates and the
	// remote ladyM endpoint so bridges checkpoint and remember against the
	// organization's infrastructure — never a local file the user picked.
	if o.CheckpointPGHost != "" {
		out.CheckpointPGHost = o.CheckpointPGHost
		out.CheckpointPGPort = o.CheckpointPGPort
		out.CheckpointPGDB = o.CheckpointPGDB
		out.CheckpointPGUser = o.CheckpointPGUser
	}
	if o.MemoryPGHost != "" {
		out.MemoryPGHost = o.MemoryPGHost
		out.MemoryPGPort = o.MemoryPGPort
		out.MemoryPGDB = o.MemoryPGDB
		out.MemoryPGUser = o.MemoryPGUser
	}
	if o.LadyMEndpoint != "" {
		out.LadyMEndpoint = o.LadyMEndpoint
	}
	if o.LadyMAPIUsername != "" {
		out.LadyMAPIUsername = o.LadyMAPIUsername
	}
	// BuiltinPlugins rides the overlay as the enterprise plugin set: an
	// empty/absent list means "not managed" (same zero-value constraint as
	// the bool fields above), so an org cannot force plugins OFF via the
	// overlay — only pin a minimum set. Passwords stay secrets (the
	// encrypted store), never overlay fields.
	if len(o.BuiltinPlugins) > 0 {
		out.BuiltinPlugins = o.BuiltinPlugins
	}
	// Bool fields (LadyMAutoRetain, LadyMSystem2 — like DebugLog) cannot be
	// overlay-managed: false == unset, so a managed "false" is indistinguishable
	// from an unmanaged field and therefore cannot be locked either.
}

// lockedFields lists the setting keys the overlay currently manages. The
// settings API surfaces them so the frontend can render read-only fields
// with their source (product design §3.1: source = "managed", locked).
func (m *managedState) lockedFields() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.overlay == nil {
		return nil
	}
	o := m.overlay
	var out []string
	add := func(key, v string) {
		if v != "" {
			out = append(out, key)
		}
	}
	add("provider", o.Provider)
	add("model", o.Model)
	add("anthropic_base_url", o.AnthropicBaseURL)
	add("openai_base_url", o.OpenAIBaseURL)
	add("checkpoint_backend", o.CheckpointBackend)
	add("memory_backend", o.MemoryBackend)
	add("checkpoint_dir", o.CheckpointDir)
	add("output_style", o.OutputStyle)
	add("memory_engine", o.MemoryEngine)
	add("model_whitelist", o.ModelWhitelist)
	add("checkpoint-pg-host", o.CheckpointPGHost)
	add("memory-pg-host", o.MemoryPGHost)
	add("ladym-endpoint", o.LadyMEndpoint)
	add("ladym-api-username", o.LadyMAPIUsername)
	if len(o.BuiltinPlugins) > 0 {
		out = append(out, "builtin-plugins")
	}
	if o.MaxTokens != 0 {
		out = append(out, "max_tokens")
	}
	if o.Temperature != 0 {
		out = append(out, "temperature")
	}
	return out
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
		{"model_whitelist", o.ModelWhitelist, cfg.ModelWhitelist},
		{"checkpoint-pg-host", o.CheckpointPGHost, cfg.CheckpointPGHost},
		{"memory-pg-host", o.MemoryPGHost, cfg.MemoryPGHost},
		{"ladym-endpoint", o.LadyMEndpoint, cfg.LadyMEndpoint},
		{"ladym-api-username", o.LadyMAPIUsername, cfg.LadyMAPIUsername},
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
			return fmt.Errorf("%w: %s is managed and locked (set to %s)", ErrManagedLocked, c.key, c.managed)
		}
	}
	if o.MaxTokens != 0 && cfg.MaxTokens != 0 && cfg.MaxTokens != o.MaxTokens {
		return fmt.Errorf("%w: max_tokens (set to %d)", ErrManagedLocked, o.MaxTokens)
	}
	if o.Temperature != 0 && cfg.Temperature != 0 && cfg.Temperature != o.Temperature {
		return fmt.Errorf("%w: temperature (set to %v)", ErrManagedLocked, o.Temperature)
	}
	if o.LadyMTopK != 0 && cfg.LadyMTopK != 0 && cfg.LadyMTopK != o.LadyMTopK {
		return fmt.Errorf("%w: ladym_topk (set to %d)", ErrManagedLocked, o.LadyMTopK)
	}
	if o.LadyMSystem2Interval != 0 && cfg.LadyMSystem2Interval != 0 && cfg.LadyMSystem2Interval != o.LadyMSystem2Interval {
		return fmt.Errorf("%w: ladym_system2_interval (set to %d)", ErrManagedLocked, o.LadyMSystem2Interval)
	}
	if len(o.BuiltinPlugins) > 0 && len(cfg.BuiltinPlugins) > 0 && !slices.Equal(o.BuiltinPlugins, cfg.BuiltinPlugins) {
		return fmt.Errorf("%w: builtin-plugins (set to %v)", ErrManagedLocked, o.BuiltinPlugins)
	}
	// LadyMAutoRetain and LadyMSystem2 (bools, like DebugLog) are not
	// lockable here: a managed false is indistinguishable from unset.
	return nil
}
