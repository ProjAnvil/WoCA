package mcp

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// ServerEntry is one mcpServers entry from a Claude-style MCP config
// (~/.claude.json's global servers or a plugin's .mcp.json).
type ServerEntry struct {
	Name    string
	Type    string // http | sse | stdio
	URL     string
	Command string // display form: command + args, no env
	HasEnv  bool   // whether any env values are set (values stay private)

	Args   []string
	Env    map[string]string
	Inline string // raw command when Claude stored a shell line
}

// ParseServerEntry decodes one mcpServers entry across the shapes found in
// Claude configs: remote servers carry type http|sse + url; local ones carry
// command + args (+ env), with the type omitted.
func ParseServerEntry(name string, raw json.RawMessage) (ServerEntry, error) {
	var cfg struct {
		Type    string            `json:"type"`
		URL     string            `json:"url"`
		Command string            `json:"command"`
		Args    []string          `json:"args"`
		Env     map[string]string `json:"env"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return ServerEntry{}, err
	}
	srv := ServerEntry{
		Name:   name,
		Type:   cfg.Type,
		URL:    cfg.URL,
		HasEnv: len(cfg.Env) > 0,
		Args:   cfg.Args,
		Env:    cfg.Env,
		Inline: strings.TrimSpace(cfg.Command),
	}
	switch {
	case cfg.Type == "http", cfg.Type == "sse":
		if srv.URL == "" {
			return ServerEntry{}, fmt.Errorf("server %s: missing url", name)
		}
	case cfg.Type == "stdio", cfg.Type == "" && cfg.Command != "":
		srv.Type = "stdio"
		srv.Command = shellQuote(cfg.Command, cfg.Args)
	default:
		return ServerEntry{}, fmt.Errorf("server %s: unsupported shape", name)
	}
	return srv, nil
}

// Spec renders the entry as a woca --mcp spec (name=command or name=url).
func (s ServerEntry) Spec() string {
	if s.URL != "" {
		return s.Name + "=" + s.URL
	}
	parts := make([]string, 0, len(s.Env)+2)
	for _, k := range sortedKeys(s.Env) {
		// sh -c prefix assignment: only the value is quoted.
		parts = append(parts, k+"="+shellWord(s.Env[k]))
	}
	cmd := s.Inline
	if len(s.Args) > 0 {
		cmd = shellQuote(cmd, s.Args)
	}
	// A stored shell line without args runs as-is under sh -c.
	parts = append(parts, cmd)
	return s.Name + "=" + strings.Join(parts, " ")
}

func sortedKeys(m map[string]string) []string {
	return slices.Sorted(maps.Keys(m))
}

// shellSpecials are the characters that force quoting in an sh -c word.
const shellSpecials = " \t\n\"'\\`$&();|<>{}*?[]~^#"

// shellQuote joins a command and its args into one sh -c line, quoting
// pieces that contain whitespace or specials.
func shellQuote(command string, args []string) string {
	parts := []string{command}
	parts = append(parts, args...)
	out := make([]string, len(parts))
	for i, p := range parts {
		out[i] = shellWord(p)
	}
	return strings.Join(out, " ")
}

func shellWord(s string) string {
	if s == "" {
		return "''"
	}
	if !strings.ContainsAny(s, shellSpecials) && !strings.HasPrefix(s, "~") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
