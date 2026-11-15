package agent

// Session grant keys (design §3.3): an "allow for session" approval answer
// stores one grant per rule-shaped key, and every later gate evaluation
// consults the session's grants AFTER the explicit permission rules (deny and
// ask always win) and BEFORE the mode arms. Only tools with a natural
// rule dimension are grantable; everything else gets "" and is never offered
// a session grant.

import (
	"net/url"
	"strings"

	"github.com/projanvil/langchain-golang/core/messages"
)

// GrantKey derives the session-grant key for a tool call:
//
//   - Bash: "bash:" + the first two whitespace-separated segments of the
//     command + "*" — a prefix wildcard, so approving "git push --force"
//     grants "git push ..." for the session ("bash:git push*").
//   - WebFetch: "domain:" + the url argument's host, port stripped — the
//     site, not the exact page ("domain:example.com").
//   - WebSearch: "websearch:" + the first two words of the query + "*"
//     ("websearch:latest AI*").
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
	}
	return ""
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

// domainKey returns "domain:" + the url's host (port stripped); missing or
// unparseable urls return "".
func domainKey(v any) string {
	raw, _ := v.(string)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	return "domain:" + u.Hostname()
}
