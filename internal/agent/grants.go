package agent

// Session grant keys (design §3.3): an "allow for session" approval answer
// stores one grant per rule-shaped key, and every later gate evaluation
// consults the session's grants AFTER the explicit permission rules (deny and
// ask always win) and BEFORE the mode arms. Only tools with a natural
// rule dimension are grantable; everything else gets "" and is never offered
// a session grant.

import (
	"net/url"
	"path/filepath"
	"strings"

	"github.com/projanvil/langchain-golang/core/messages"
)

// GrantKey derives the session-grant key for a tool call:
//
//   - Bash: "bash:" + the first two whitespace-separated segments of the
//     command + "*" — a prefix wildcard, so approving "git push --force"
//     grants "git push ..." for the session ("bash:git push*").
//   - WebFetch: "domain:" + the url argument's host, port stripped and
//     lowercased — the site, not the exact page ("domain:example.com").
//   - WebSearch: "websearch:" + the first two words of the query + "*"
//     ("websearch:latest AI*").
//   - Write/Edit/NotebookEdit: "file:" + the path's first segment — the
//     directory (with a "/*" wildcard when the path descends), the bare
//     name for a root file ("file:src/*", "file:notes.txt"). Paths that
//     escape the workspace ("..") or arrive absolute are not grantable.
//
// Any other tool — and any call whose key dimension is missing or
// unparseable — returns "", meaning no session grant can be offered for it.
func GrantKey(call messages.ToolCall) string {
	switch call.Name {
	case "Bash":
		return prefixKey("bash", firstWords(call.Args["command"], 2))
	case "WebFetch":
		return domainKey(call.Args["url"])
	case "WebSearch":
		return prefixKey("websearch", firstWords(call.Args["query"], 2))
	case "Write", "Edit":
		return fileGrantKey(call.Args["file_path"])
	case "NotebookEdit":
		return fileGrantKey(call.Args["notebook_path"])
	}
	return ""
}

// fileGrantKey derives "file:" + the cleaned first path segment: a
// descending path grants its whole top directory ("src/a.go" →
// "file:src/*"), a root file grants exactly itself ("notes.txt" →
// "file:notes.txt"). Escaping ("..") or absolute paths return "" — a grant
// must stay inside the workspace.
func fileGrantKey(v any) string {
	s, _ := v.(string)
	if s == "" {
		return ""
	}
	cleaned := filepath.ToSlash(filepath.Clean(s))
	head, rest, _ := strings.Cut(cleaned, "/")
	if head == "" || head == "." || head == ".." {
		return ""
	}
	if rest != "" {
		return "file:" + head + "/*"
	}
	return "file:" + head
}

// prefixKey renders "" when words is empty (nothing grantable), otherwise
// prefix + ":" + the space-joined words + the trailing wildcard.
func prefixKey(prefix string, words []string) string {
	if len(words) == 0 {
		return ""
	}
	return prefix + ":" + strings.Join(words, " ") + "*"
}

// firstWords returns the first n whitespace-separated segments of v when v is
// a non-empty string, else nil.
func firstWords(v any, n int) []string {
	s, _ := v.(string)
	fields := strings.Fields(s)
	if len(fields) > n {
		fields = fields[:n]
	}
	return fields
}

// domainKey returns "domain:" + the url's host (port stripped, lowercased —
// hosts are case-insensitive, so a grant stored from one casing matches every
// later casing of the same site); missing or unparseable urls return "".
func domainKey(v any) string {
	raw, _ := v.(string)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	return "domain:" + strings.ToLower(u.Hostname())
}
