// Package permissions implements tool permission rules:
// an allow/deny/ask list read from a JSON settings file, plus the matcher that
// resolves a tool call to its effective action.
package permissions

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
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
		if matchRule(rule, toolName, inputJSON) {
			return ActionDeny, true
		}
	}
	for _, rule := range r.Ask {
		if matchRule(rule, toolName, inputJSON) {
			return ActionAsk, true
		}
	}
	for _, rule := range r.Allow {
		if matchRule(rule, toolName, inputJSON) {
			return ActionAllow, true
		}
	}
	return "", false
}

// matchRule reports whether a single rule string matches a tool call. A rule
// takes one of three forms, tested in this order:
//
//   - "Tool(glob)" — e.g. "Bash(git *)": the tool name must equal the part
//     before the parenthesis, and the glob must match the JSON-serialized
//     input map.
//   - an exact tool name — e.g. "Bash", "Read".
//   - a regex over the tool name, matched fully anchored — e.g. "Edit|Write".
func matchRule(rule, toolName, inputJSON string) bool {
	if name, glob, ok := splitToolGlob(rule); ok {
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
	for i := 0; i < len(s); i++ {
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
