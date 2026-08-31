package tui

import (
	"context"
	"strings"
)

// Composer completions, mirroring the web grammar exactly (web/src/
// composables/useAutocomplete.ts): `/` at line start completes console
// commands plus loaded skills; `@` at line start or after whitespace
// completes subagent definitions. The inventories come from the same agent
// methods the web's /api/completions endpoint calls, so the two frontends
// never drift.

// completion groups order the merged menu.
const (
	groupCommands = "commands"
	groupSkills   = "skills"
	groupAgents   = "agents"
)

// refreshCompletions reloads the skill and subagent inventories (cheap
// directory reads; refreshed after every submitted turn so edits to
// .woca/skills or agent definitions show up without a restart).
func (m *Model) refreshCompletions() {
	if m.agent == nil {
		return
	}
	ctx := context.Background()
	m.skillList = m.agent.SkillsIn(ctx)
	m.agentList = m.agent.AgentDefinitionsIn(ctx)
}

// atTrigger reports the byte offset of a trailing `@token` (at line start
// or after whitespace), or -1 when the input does not end in one.
func atTrigger(input string) int {
	i := strings.LastIndexByte(input, '@')
	if i < 0 {
		return -1
	}
	if i > 0 && input[i-1] != ' ' {
		return -1
	}
	rest := input[i+1:]
	if rest == "" {
		return i
	}
	for j := 0; j < len(rest); j++ {
		c := rest[j]
		isAlnum := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
		if !isAlnum && c != '_' && c != '-' {
			return -1
		}
	}
	return i
}

// allSuggestions is the composer menu: `@` inputs list subagents, `/`
// inputs list console commands plus skills, anything else lists nothing.
func (m *Model) allSuggestions(input string) []command {
	if i := atTrigger(input); i >= 0 {
		prefix := strings.ToLower(input[i:])
		var out []command
		for _, d := range m.agentList {
			name := "@" + d.Name
			if strings.HasPrefix(strings.ToLower(name), prefix) {
				out = append(out, command{name: name, desc: d.Description, group: groupAgents})
			}
		}
		return out
	}
	if !strings.HasPrefix(input, "/") {
		return nil
	}
	out := suggestions(input)
	for _, s := range m.skillList {
		name := "/" + s.Name
		if strings.HasPrefix(strings.ToLower(name), strings.ToLower(input)) {
			out = append(out, command{name: name, desc: s.Description, group: groupSkills})
		}
	}
	return out
}

// acceptCompletion replaces the active trigger token with the chosen entry:
// `/` entries replace the whole input prefix, `@` entries replace the
// trailing token and leave a space for further typing.
func (m *Model) acceptCompletion(entry string) {
	if i := atTrigger(m.input); i >= 0 && strings.HasPrefix(entry, "@") {
		m.input = m.input[:i] + entry + " "
		m.cursor = len(m.input)
		m.sel = 0
		return
	}
	m.input = entry
	m.cursor = len(m.input)
	m.sel = 0
}

// completionsMenuVisible reports whether the current input shows a menu.
func (m *Model) completionsMenuVisible() bool {
	return len(m.allSuggestions(m.input)) > 0
}
