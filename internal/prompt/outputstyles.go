package prompt

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
)

// Output styles (a port of Claude Code's outputStyles): a personality section
// appended to the system prompt that changes how responses are delivered —
// same tools and knowledge, different presentation contract.

// Style is one output style preset.
type Style struct {
	Name        string
	Description string
	// Text is appended to the system prompt; empty means no extra section.
	Text string
}

// Styles are the built-in output styles.
var Styles = []Style{
	{Name: "default", Description: "normal coding-assistant behavior"},
	{Name: "explanatory", Description: "answer first, then explain the reasoning", Text: `# Output style: Explanatory

Provide the best possible answer to the user's request first. Then add a brief explanation section covering:
1. How you arrived at that answer (what you read, ran, or inferred)
2. Key concepts the user should know to understand or verify it

Keep the explanation tight — a few sentences, not a tutorial. If the request is trivial (a one-line factual answer), skip the explanation.`},
	{Name: "learning", Description: "Socratic mode — guide instead of solving", Text: `# Output style: Learning

The user is learning and wants to be guided, not rescued. For every request:
1. Assess what they already know from the conversation.
2. Give them enough to take the next step themselves: a hint, a pointed question, or a sketch of the approach — not the finished solution, unless they explicitly ask for it or are clearly stuck after trying.
3. When you do provide code, annotate why it works, not just what it does.
4. When they make a mistake, ask what they expected before correcting it.

Balance: never withhold help to make a point; step in when they are blocked or frustrated.`},
}

// FindStyle returns the style with the given name; "" and "default" resolve to
// the default style. ok=false for unknown names.
func FindStyle(name string) (Style, bool) {
	if name == "" {
		name = "default"
	}
	for _, s := range Styles {
		if s.Name == name {
			return s, true
		}
	}
	return Style{}, false
}

// StyleNames lists the available style names in order.
func StyleNames() []string {
	out := make([]string, 0, len(Styles))
	for _, s := range Styles {
		out = append(out, s.Name)
	}
	return out
}

// Segment caching (Claude Code's systemPromptSections cache): dynamic prompt
// segments — project memory files, environment — are cached by content key so
// repeated builds (style switches, /clear, /compact) produce byte-identical
// prompts without re-reading disk, keeping provider prompt caches valid.

var (
	segmentCache sync.Map
	cacheHits    atomic.Int64
	cacheMisses  atomic.Int64
)

// CacheStats reports segment-cache counters (tests/diagnostics).
func CacheStats() (hits, misses int64) {
	return cacheHits.Load(), cacheMisses.Load()
}

// cachedSegment returns the cached value for key, building it on miss.
func cachedSegment(key string, build func() string) string {
	if v, ok := segmentCache.Load(key); ok {
		cacheHits.Add(1)
		return v.(string)
	}
	s := build()
	segmentCache.Store(key, s)
	cacheMisses.Add(1)
	return s
}

// resetSegmentCache drops all cached segments (tests).
func resetSegmentCache() {
	segmentCache = sync.Map{}
	cacheHits.Store(0)
	cacheMisses.Store(0)
}

// memorySection renders one instruction-file section, cached by
// path/size/modtime/label so unchanged files are never re-read. Kind and
// Label participate in the key: the same file can render under different
// headers in the git-root-equals-$HOME edge case.
func memorySection(mf memoryFile) (string, bool) {
	st, err := os.Stat(mf.Path)
	if err != nil || st.IsDir() {
		return "", false
	}
	section := cachedSegment(fmt.Sprintf("%s|%d|%d|%s|%s", mf.Path, st.Size(), st.ModTime().UnixNano(), mf.Kind, mf.Label), func() string {
		data, err := os.ReadFile(mf.Path)
		if err != nil {
			return ""
		}
		content := strings.TrimSpace(string(data))
		if content == "" {
			return ""
		}
		return "\n\n# " + mf.Kind + " instructions (" + mf.Label + ")\n\n" + content
	})
	return section, section != ""
}
