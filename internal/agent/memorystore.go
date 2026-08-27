package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/memory"
	"github.com/projanvil/woca/internal/settings"
	"github.com/projanvil/woca/internal/store"
)

// Relevant-memory prefetch and attachments: at the start of each
// model turn the memory store is searched asynchronously for entries relevant
// to the latest user message; after each tool round the settled, not-yet-
// injected entries are attached as <relevant-memory> messages after the
// result batch, so the model sees long-term context it would otherwise miss.
// Relevance is deliberately naive (keyword containment), serving the role of
// a memory search.
//
// The subsystem lives in memoryRuntime, a value component of Agent: it owns
// the engine, the KV-store fallback, the per-turn prefetch state and the
// System 2 throttle, and takes everything else (settings loader, workspace,
// the obs log sink) as injected inputs. mu guards all fields; engine calls
// (IO) always happen outside the lock, on a snapshot.

// maxMemoryAttachments bounds attachments per tool round.
const maxMemoryAttachments = 5

// memoryPrefetch is the turn-scoped prefetch state.
type memoryPrefetch struct {
	entries    []store.Entry
	engineHits []memory.Hit
	injected   map[string]bool // keys already attached (per agent lifetime)
}

// injectedMemoryLimit bounds the injected-key set.
const injectedMemoryLimit = 256

// memoryRuntime is the long-term-memory subsystem (see the block comment
// above). A nil engine disables it; the workspace fallback (agent workdir)
// is resolved by the host, not here — Begin and the retain methods take the
// already-resolved workspace as a parameter.
type memoryRuntime struct {
	mu           sync.Mutex
	engine       memory.Engine
	store        *store.Store // keyword-match fallback when engine is nil
	workspace    string       // explicit override ("" = none)
	prefetch     memoryPrefetch
	lastSystem2  map[string]int64 // per-workspace throttle (unix seconds)
	loadSettings func() settings.Settings
	log          func(level, event string, fields map[string]any) // obs sink, nil-safe
}

// init wires the runtime at Agent construction (idempotent, lock-guarded).
func (m *memoryRuntime) init(store *store.Store, engine memory.Engine, loadSettings func() settings.Settings, log func(string, string, map[string]any)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.store = store
	m.engine = engine
	m.loadSettings = loadSettings
	m.log = log
}

// Engine snapshots the current engine (nil = disabled).
func (m *memoryRuntime) Engine() memory.Engine {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.engine
}

// SetEngine swaps the engine whole-pointer (settings rebuild). In-flight
// calls keep the old engine; the caller delays Close.
func (m *memoryRuntime) SetEngine(e memory.Engine) {
	m.mu.Lock()
	m.engine = e
	m.mu.Unlock()
}

// SetWorkspace records an explicit workspace override (empty ignored).
func (m *memoryRuntime) SetWorkspace(ws string) {
	if ws == "" {
		return
	}
	m.mu.Lock()
	m.workspace = ws
	m.mu.Unlock()
}

// Workspace reports the explicit override, if any.
func (m *memoryRuntime) Workspace() (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.workspace, m.workspace != ""
}

// settings returns the persisted settings (zero value when no loader).
func (m *memoryRuntime) settings() settings.Settings {
	m.mu.Lock()
	loader := m.loadSettings
	m.mu.Unlock()
	if loader == nil {
		return settings.Settings{}
	}
	return loader()
}

// warn logs a best-effort observability event; the memory subsystem must
// never fail a run, so failures are dropped after being logged.
func (m *memoryRuntime) warn(event string, fields map[string]any) {
	m.mu.Lock()
	log := m.log
	m.mu.Unlock()
	if log != nil {
		log("warn", event, fields)
	}
}

// Begin asynchronously searches long-term memory for entries relevant to the
// last user message. With an engine wired it runs semantic Recall
// workspace-scoped (natural-language query, settings topK); otherwise it
// falls back to the KV keyword match. The result is consumed non-blockingly
// after tool rounds (TakeAttachments).
func (m *memoryRuntime) Begin(ctx context.Context, msgs []messages.Message, ws string) {
	topK := settings.DefaultLadyMTopK
	if k := m.settings().LadyMTopK; k > 0 {
		topK = k
	}
	m.mu.Lock()
	eng := m.engine
	kv := m.store
	m.mu.Unlock()
	if eng != nil {
		query := lastHumanQuery(msgs)
		if query == "" {
			return
		}
		go func() {
			hits, err := eng.Recall(context.WithoutCancel(ctx), query, ws, topK)
			if err != nil {
				return
			}
			if scope := memoryScopeFrom(ctx); scope != nil {
				scope.mu.Lock()
				scope.engineHits = hits
				scope.mu.Unlock()
				return
			}
			m.mu.Lock()
			m.prefetch.engineHits = hits
			m.mu.Unlock()
		}()
		return
	}
	if kv == nil {
		return
	}
	terms := memoryTerms(msgs)
	if len(terms) == 0 {
		return
	}
	go func() {
		entries, err := kv.List(context.WithoutCancel(ctx), "", "")
		if err != nil {
			return
		}
		var hits []store.Entry
		for _, e := range entries {
			value := strings.ToLower(string(e.Value))
			for _, term := range terms {
				if strings.Contains(value, term) || strings.Contains(strings.ToLower(e.Namespace+"/"+e.Key), term) {
					hits = append(hits, e)
					break
				}
			}
			if len(hits) >= maxMemoryAttachments*2 {
				break
			}
		}
		if scope := memoryScopeFrom(ctx); scope != nil {
			scope.mu.Lock()
			scope.entries = hits
			scope.mu.Unlock()
			return
		}
		m.mu.Lock()
		m.prefetch.entries = hits
		m.mu.Unlock()
	}()
}

// TakeAttachments drains the run's prefetch results into <relevant-memory>
// human messages. The per-run scope (ctx) wins; without one it falls back
// to the agent-level slot.
func (m *memoryRuntime) TakeAttachments(ctx context.Context) []messages.Message {
	if scope := memoryScopeFrom(ctx); scope != nil {
		scope.mu.Lock()
		entries, hits := scope.entries, scope.engineHits
		scope.entries, scope.engineHits = nil, nil
		scope.mu.Unlock()
		return m.buildAttachments(entries, hits)
	}
	m.mu.Lock()
	entries, hits := m.prefetch.entries, m.prefetch.engineHits
	m.prefetch.entries, m.prefetch.engineHits = nil, nil
	m.mu.Unlock()
	return m.buildAttachments(entries, hits)
}

// buildAttachments formats settled prefetch results into <relevant-memory>
// human messages. The injected dedup table is agent-level shared state and
// stays under m.mu: concurrent runs must not mark it outside the lock (Go
// fatals on concurrent map writes).
func (m *memoryRuntime) buildAttachments(entries []store.Entry, hits []memory.Hit) []messages.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.prefetch.injected == nil {
		m.prefetch.injected = map[string]bool{}
	}
	injected := m.prefetch.injected

	var out []messages.Message
	for _, h := range hits {
		if len(out) >= maxMemoryAttachments {
			break
		}
		if injected[h.ID] {
			continue
		}
		injected[h.ID] = true
		summary := strings.TrimSpace(h.Summary)
		content := strings.TrimSpace(h.Content)
		if summary == "" && content == "" {
			continue
		}
		if len(content) > 2000 {
			content = clampUTF8(content, 2000) + "…"
		}
		line := "[" + h.Layer + "|" + h.Type + "|" + fmt.Sprintf("%.2f", h.Score)
		if age := memoryAge(h.UpdatedAt); age != "" {
			line += "|" + age
		}
		line += "] " + summary
		if content != "" && content != summary {
			line += "\n" + content
		}
		out = append(out, messages.Human(
			"<relevant-memory>\n"+line+"\n</relevant-memory>"))
	}
	for _, e := range entries {
		if len(out) >= maxMemoryAttachments {
			break
		}
		id := e.Namespace + "/" + e.Key
		if injected[id] {
			continue
		}
		injected[id] = true
		value := strings.TrimSpace(trimJSONString(e.Value))
		if value == "" {
			continue
		}
		if len(value) > 2000 {
			value = clampUTF8(value, 2000) + "…"
		}
		out = append(out, messages.Human(
			"<relevant-memory>\n"+id+"\n"+value+"\n</relevant-memory>"))
	}
	if len(injected) > injectedMemoryLimit {
		m.prefetch.injected = map[string]bool{} // reset rather than grow unbounded
	}
	return out
}

// lastHumanQuery returns the last human message text (the recall query),
// clamped to 2000 characters. Unlike memoryTerms it keeps the natural
// language: semantic recall wants the sentence, not the keywords.
func lastHumanQuery(msgs []messages.Message) string {
	last := ""
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == messages.RoleHuman {
			last = msgs[i].Content
			break
		}
	}
	return strings.TrimSpace(clampUTF8(last, 2000))
}

// memoryAge renders a hit's UpdatedAt as a coarse relative age for the
// attachment header, so the model can arbitrate when a fresh fact
// contradicts a stale one. Engines that report no timestamp (the KV-store
// fallback, older remotes) yield "" and the age field is omitted entirely.
func memoryAge(ts float64) string {
	if ts <= 0 {
		return ""
	}
	d := time.Since(time.Unix(int64(ts), 0))
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	case d < 365*24*time.Hour:
		return fmt.Sprintf("%dmo ago", int(d.Hours()/(24*30)))
	default:
		return fmt.Sprintf("%dy ago", int(d.Hours()/(24*365)))
	}
}

// clampUTF8 truncates s to at most n bytes, backing off to a rune boundary
// so a multi-byte character is never split. n < 0 yields "".
func clampUTF8(s string, n int) string {
	if n < 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// trimJSONString unwraps a JSON-encoded string value ("\"text\"") when
// present.
func trimJSONString(raw []byte) string {
	s := string(raw)
	if strings.HasPrefix(s, "\"") {
		var unquoted string
		if err := json.Unmarshal(raw, &unquoted); err == nil {
			return unquoted
		}
	}
	return s
}

// memoryTerms extracts lowercase keywords (>3 chars, letters/digits) from the
// last human message.
func memoryTerms(msgs []messages.Message) []string {
	last := ""
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == messages.RoleHuman {
			last = msgs[i].Content
			break
		}
	}
	if last == "" {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, field := range strings.FieldsFunc(strings.ToLower(last), func(r rune) bool {
		return !('a' <= r && r <= 'z' || '0' <= r && r <= '9' || r >= 0x80)
	}) {
		if len(field) < 4 || seen[field] {
			continue
		}
		seen[field] = true
		out = append(out, field)
		if len(out) >= 12 {
			break
		}
	}
	return out
}
