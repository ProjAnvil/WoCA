package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/memory"
	"github.com/projanvil/woca/internal/settings"
	"github.com/projanvil/woca/internal/store"
)

// Relevant-memory prefetch and attachments, a port of Claude Code's
// startRelevantMemoryPrefetch + stage 27 consumption: at the start of each
// model turn the memoryStore is searched asynchronously for entries relevant
// to the latest user message; after each tool round the settled, not-yet-
// injected entries are attached as <relevant-memory> messages after the
// result batch, so the model sees long-term context it would otherwise miss.
// Relevance is deliberately naive (keyword containment) — the woca
// equivalent of Claude Code's memory search.

// maxMemoryAttachments bounds attachments per tool round.
const maxMemoryAttachments = 5

// memoryPrefetch is the turn-scoped prefetch state.
type memoryPrefetch struct {
	mu         sync.Mutex
	entries    []store.Entry
	engineHits []memory.Hit
	injected   map[string]bool // keys already attached (per agent lifetime)
}

// injectedMemoryLimit bounds the injected-key set.
const injectedMemoryLimit = 256

// beginMemoryPrefetch asynchronously searches long-term memory for entries
// relevant to the last user message. With a memory engine wired it runs
// semantic Recall workspace-scoped (natural-language query, settings topK);
// otherwise it falls back to the KV keyword match. The result is consumed
// non-blockingly after tool rounds.
func (a *Agent) beginMemoryPrefetch(ctx context.Context, msgs []messages.Message) {
	if eng := a.MemoryEngine(); eng != nil {
		query := lastHumanQuery(msgs)
		if query == "" {
			return
		}
		topK := settings.DefaultLadyMTopK
		if a.loadSettings != nil {
			if k := a.loadSettings().LadyMTopK; k > 0 {
				topK = k
			}
		}
		ws := a.memoryWorkspaceCtx(ctx)
		go func() {
			hits, err := eng.Recall(context.WithoutCancel(ctx), query, ws, topK)
			if err != nil {
				return
			}
			a.memPrefetch.mu.Lock()
			a.memPrefetch.engineHits = hits
			a.memPrefetch.mu.Unlock()
		}()
		return
	}
	if a.memory == nil {
		return
	}
	terms := memoryTerms(msgs)
	if len(terms) == 0 {
		return
	}
	mem := a.memory
	go func() {
		entries, err := mem.List(context.WithoutCancel(ctx), "", "")
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
		a.memPrefetch.mu.Lock()
		a.memPrefetch.entries = hits
		a.memPrefetch.mu.Unlock()
	}()
}

// takeMemoryAttachments returns attachment messages for settled, relevant,
// not-yet-injected memory entries (non-blocking; nothing when the prefetch
// has not settled).
func (a *Agent) takeMemoryAttachments() []messages.Message {
	a.memPrefetch.mu.Lock()
	entries := a.memPrefetch.entries
	a.memPrefetch.entries = nil
	hits := a.memPrefetch.engineHits
	a.memPrefetch.engineHits = nil
	injected := a.memPrefetch.injected
	a.memPrefetch.mu.Unlock()
	if injected == nil {
		injected = map[string]bool{}
	}

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
		line := fmt.Sprintf("[%s|%s|%.2f] %s", h.Layer, h.Type, h.Score, summary)
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
	if len(out) > 0 {
		a.memPrefetch.mu.Lock()
		a.memPrefetch.injected = injected
		if len(a.memPrefetch.injected) > injectedMemoryLimit {
			a.memPrefetch.injected = map[string]bool{} // reset rather than grow unbounded
		}
		a.memPrefetch.mu.Unlock()
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

// memoryTerms extracts lowercase keywords (>3 chars, letters/digits) from
// the last human message.
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
