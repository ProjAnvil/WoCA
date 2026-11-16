// Package permissions implements tool permission rules:
// an allow/deny/ask list read from a JSON settings file, plus the matcher that
// resolves a tool call to its effective action.
package permissions

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Action is the effective permission decision for a tool call.
type Action string

const (
	ActionAllow Action = "allow"
	ActionDeny  Action = "deny"
	ActionAsk   Action = "ask"
)

// Rules holds the three permission lists. Each entry is a rule string matched
// against a tool call using the semantics documented on matchRule.
type Rules struct {
	Allow []string `json:"allow"`
	Deny  []string `json:"deny"`
	Ask   []string `json:"ask"`
}

// Load reads a JSON settings file of the form
//
//	{"permissions":{"allow":[...],"deny":[...],"ask":[...]}}
//
// and returns the permission rules it contains. A missing file yields zero
// Rules with a nil error, matching an empty permission set.
func Load(path string) (Rules, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Rules{}, nil
		}
		return Rules{}, fmt.Errorf("permissions: read %s: %w", path, err)
	}

	var wrapper struct {
		Permissions Rules `json:"permissions"`
	}
	if err := json.Unmarshal(data, &wrapper); err != nil {
		return Rules{}, fmt.Errorf("permissions: parse %s: %w", path, err)
	}
	return wrapper.Permissions, nil
}

// Action returns the effective action for a tool call, honoring the precedence
// deny > ask > allow. It reports ("", false) when no rule matches.
func (r Rules) Action(toolName string, input map[string]any) (Action, bool) {
	serialized, err := json.Marshal(input)
	if err != nil {
		serialized = []byte("null")
	}
	inputJSON := string(serialized)

	for _, rule := range r.Deny {
		if matchRule(rule, toolName, input, inputJSON) {
			return ActionDeny, true
		}
	}
	for _, rule := range r.Ask {
		if matchRule(rule, toolName, input, inputJSON) {
			return ActionAsk, true
		}
	}
	for _, rule := range r.Allow {
		if matchRule(rule, toolName, input, inputJSON) {
			return ActionAllow, true
		}
	}
	return "", false
}

// matchRule reports whether a single rule string matches a tool call. A rule
// takes one of three forms, tested in this order:
//
//   - "Tool(glob)" — e.g. "Bash(git *)": the tool name must equal the part
//     before the parenthesis. For Bash and the web tools the glob matches
//     the JSON-serialized input map. For the file tools the glob matches
//     the call's path argument (see filePlanePath), and the virtual names
//     FileWrite/FileRead cover the write/read operation classes.
//   - an exact tool name — e.g. "Bash", "Read".
//   - a regex over the tool name, matched fully anchored — e.g. "Edit|Write".
func matchRule(rule, toolName string, input map[string]any, inputJSON string) bool {
	if name, glob, ok := splitToolGlob(rule); ok {
		if arg, applicable := filePlanePath(name, toolName, input); applicable {
			if arg == "" {
				return false // a file-plane call without its path argument never matches
			}
			return pathGlobMatch(glob, normalizePath(arg))
		}
		return name == toolName && globMatch(glob, inputJSON)
	}
	if rule == toolName {
		return true
	}
	re, err := regexp.Compile("^(?:" + rule + ")$")
	if err != nil {
		return false
	}
	return re.MatchString(toolName)
}

// fileToolPathArgs maps every tool whose rule dimension is a filesystem path
// to the argument carrying that path.
var fileToolPathArgs = map[string]string{
	"Read":         "file_path",
	"Write":        "file_path",
	"Edit":         "file_path",
	"NotebookEdit": "notebook_path",
	"Glob":         "path",
	"Grep":         "path",
	"ListDir":      "path",
}

// planeMembers maps the virtual file-plane rule names to the tools they
// cover: FileWrite is the write operation class, FileRead the read class
// (deepagents' permission model).
var planeMembers = map[string][]string{
	"FileWrite": {"Write", "Edit", "NotebookEdit"},
	"FileRead":  {"Read", "Glob", "Grep", "ListDir"},
}

// filePlanePath resolves the path a "Tool(glob)" rule's glob matches when
// the rule names the file plane: the called tool's own path argument for a
// direct file-tool rule, or the member tool's path argument when the rule
// names FileWrite/FileRead. ("", false) leaves the rule to the legacy
// JSON-blob form; ("", true) means the call is on the file plane but
// carries no path (never a match).
func filePlanePath(name, toolName string, input map[string]any) (string, bool) {
	arg := ""
	switch {
	case fileToolPathArgs[name] != "" && name == toolName:
		arg = fileToolPathArgs[name]
	default:
		if !slices.Contains(planeMembers[name], toolName) {
			return "", false
		}
		arg = fileToolPathArgs[toolName]
	}
	raw, _ := input[arg].(string)
	return raw, true
}

// normalizePath cleans a tool-supplied path into the slash-separated form
// path globs match against.
func normalizePath(p string) string {
	return filepath.ToSlash(filepath.Clean(p))
}

// pathGlobMatch reports whether glob matches path (both slash-separated and
// cleaned). "*", "?" and "[...]" never cross a separator; "**" spans zero or
// more whole segments. The glob must match the whole path.
func pathGlobMatch(glob, path string) bool {
	return matchPathSegments(strings.Split(glob, "/"), strings.Split(path, "/"))
}

// matchPathSegments matches path glob segments against path segments.
func matchPathSegments(pat, seg []string) bool {
	if len(pat) == 0 {
		return len(seg) == 0
	}
	if pat[0] == "**" {
		for i := 0; i <= len(seg); i++ {
			if matchPathSegments(pat[1:], seg[i:]) {
				return true
			}
		}
		return false
	}
	if len(seg) == 0 || !segmentMatch(pat[0], seg[0]) {
		return false
	}
	return matchPathSegments(pat[1:], seg[1:])
}

// segmentMatch matches one glob segment: "*" is anything but a separator,
// "?" one non-separator character, "[...]" (leading "!" or "^" negates) a
// character class, everything else literal.
func segmentMatch(pat, s string) bool {
	re, err := regexp.Compile(segmentRegex(pat))
	if err != nil {
		return false
	}
	return re.MatchString(s)
}

// segmentRegex compiles one glob segment into an anchored regex.
func segmentRegex(pat string) string {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pat); i++ {
		switch c := pat[i]; c {
		case '*':
			b.WriteString("[^/]*")
		case '?':
			b.WriteString("[^/]")
		case '[':
			j := i + 1
			neg := j < len(pat) && (pat[j] == '!' || pat[j] == '^')
			if neg {
				j++
			}
			body := strings.Builder{}
			for j < len(pat) && pat[j] != ']' {
				if pat[j] == '-' {
					body.WriteByte('-')
				} else {
					body.WriteString(regexp.QuoteMeta(string(pat[j])))
				}
				j++
			}
			if j >= len(pat) { // unterminated class: literal "["
				b.WriteString(regexp.QuoteMeta("["))
				continue
			}
			if body.Len() == 0 {
				b.WriteString(regexp.QuoteMeta("["))
				continue
			}
			if neg {
				b.WriteString("[^" + body.String() + "]")
			} else {
				b.WriteString("[" + body.String() + "]")
			}
			i = j
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	return b.String()
}

// splitToolGlob recognizes the "Tool(glob)" form, e.g. "Bash(git *)". It only
// applies when the part before the parenthesis is a plain tool identifier, so
// a regex containing parentheses is not mistaken for a glob rule.
func splitToolGlob(rule string) (name, glob string, ok bool) {
	if !strings.HasSuffix(rule, ")") {
		return "", "", false
	}
	open := strings.IndexByte(rule, '(')
	if open <= 0 || !isToolName(rule[:open]) {
		return "", "", false
	}
	return rule[:open], rule[open+1 : len(rule)-1], true
}

// isToolName reports whether s is a plain tool identifier: a letter followed
// by letters or digits.
func isToolName(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// globMatch reports whether the glob matches the serialized input, converting
// '*' to ".*" and '?' to "." and treating every other character literally.
func globMatch(glob, input string) bool {
	re, err := regexp.Compile(globToRegex(glob))
	if err != nil {
		return false
	}
	return re.MatchString(input)
}

// globToRegex converts a glob to an equivalent, unanchored regular expression.
func globToRegex(glob string) string {
	var b strings.Builder
	for _, r := range glob {
		switch r {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	return b.String()
}
