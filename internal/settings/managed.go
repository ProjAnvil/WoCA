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
	// Langfuse (the observability plugin, console-owned on bridges): same
	// only-true semantics as BuiltinPlugins — the console can enable and
	// configure it, not force it off on devices that configured it locally.
	if o.LangfuseEnabled {
		out.LangfuseEnabled = true
	}
	if o.LangfuseBaseURL != "" {
		out.LangfuseBaseURL = o.LangfuseBaseURL
	}
	if o.LangfusePublicKey != "" {
		out.LangfusePublicKey = o.LangfusePublicKey
	}
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
	// The canonical key spelling is the Key* constant vocabulary (the
	// settings.Apply CLI surface) — one spelling everywhere, never a mix.
	add(KeyProvider, o.Provider)
	add(KeyModel, o.Model)
	add(KeyAnthropicBaseURL, o.AnthropicBaseURL)
	add(KeyOpenAIBaseURL, o.OpenAIBaseURL)
	add(KeyCheckpoint, o.CheckpointBackend)
	add(KeyMemory, o.MemoryBackend)
	add(KeyCheckpointDir, o.CheckpointDir)
	add(KeyOutputStyle, o.OutputStyle)
	add(KeyMemoryEngine, o.MemoryEngine)
	add(KeyModelWhitelist, o.ModelWhitelist)
	add(KeyCheckpointPGHost, o.CheckpointPGHost)
	add(KeyMemoryPGHost, o.MemoryPGHost)
	add(KeyLadyMEndpoint, o.LadyMEndpoint)
	add(KeyLadyMAPIUsername, o.LadyMAPIUsername)
	if len(o.BuiltinPlugins) > 0 {
		out = append(out, KeyBuiltinPlugins)
	}
	if o.LangfuseEnabled {
		out = append(out, KeyLangfuseEnabled)
	}
	add(KeyLangfuseBaseURL, o.LangfuseBaseURL)
	add(KeyLangfusePublicKey, o.LangfusePublicKey)
	if o.MaxTokens != 0 {
		out = append(out, KeyMaxTokens)
	}
	if o.Temperature != 0 {
		out = append(out, KeyTemperature)
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
		{KeyProvider, o.Provider, cfg.Provider},
		{KeyModel, o.Model, cfg.Model},
		{KeyAnthropicBaseURL, o.AnthropicBaseURL, cfg.AnthropicBaseURL},
		{KeyOpenAIBaseURL, o.OpenAIBaseURL, cfg.OpenAIBaseURL},
		{KeyCheckpoint, o.CheckpointBackend, cfg.CheckpointBackend},
		{KeyMemory, o.MemoryBackend, cfg.MemoryBackend},
		{KeyCheckpointDir, o.CheckpointDir, cfg.CheckpointDir},
		{KeyOutputStyle, o.OutputStyle, cfg.OutputStyle},
		{KeyMemoryEngine, o.MemoryEngine, cfg.MemoryEngine},
		{KeyModelWhitelist, o.ModelWhitelist, cfg.ModelWhitelist},
		{KeyCheckpointPGHost, o.CheckpointPGHost, cfg.CheckpointPGHost},
		{KeyMemoryPGHost, o.MemoryPGHost, cfg.MemoryPGHost},
		{KeyLadyMEndpoint, o.LadyMEndpoint, cfg.LadyMEndpoint},
		{KeyLadyMAPIUsername, o.LadyMAPIUsername, cfg.LadyMAPIUsername},
		{"ladym_db_path", o.LadyMDBPath, cfg.LadyMDBPath},
		{"ladym_embedding", o.LadyMEmbedding, cfg.LadyMEmbedding},
		{"ladym_embedding_model", o.LadyMEmbeddingModel, cfg.LadyMEmbeddingModel},
		{"ladym_embedding_base_url", o.LadyMEmbeddingBaseURL, cfg.LadyMEmbeddingBaseURL},
		{"ladym_llm", o.LadyMLLM, cfg.LadyMLLM},
		{KeyLangfuseBaseURL, o.LangfuseBaseURL, cfg.LangfuseBaseURL},
		{KeyLangfusePublicKey, o.LangfusePublicKey, cfg.LangfusePublicKey},
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
	// LangfuseEnabled is only-true (a managed false is unset): locking the
	// switch means an overlay-enabled plugin cannot be turned off locally.
	if o.LangfuseEnabled && !cfg.LangfuseEnabled {
		return fmt.Errorf("%w: langfuse-enabled (enabled by the console)", ErrManagedLocked)
	}
	// LadyMAutoRetain and LadyMSystem2 (bools, like DebugLog) are not
	// lockable here: a managed false is indistinguishable from unset.
	return nil
}
