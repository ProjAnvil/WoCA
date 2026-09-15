// Package claudeenv imports the user-level Claude environment into
// woca: skills under ~/.claude/skills and the global mcpServers from
// ~/.claude.json. Selected skills are symlinked into ~/.woca/skills (so the
// existing skill loader picks them up), and selected MCP servers are
// materialized as ~/.woca/mcp.json specs consumed at agent startup. The
// checkbox selection lives in the settings database (claude-skills /
// claude-mcp).
package claudeenv

import (
	"cmp"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// homeDir is swappable in tests.
var homeDir = os.UserHomeDir

// SkillEntry is one skill detected under ~/.claude/skills.
type SkillEntry struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// Source is the absolute path of the skill directory (~/.claude/skills/<name>).
	Source string `json:"source"`
}

// MCPServer is one server detected in ~/.claude.json's global mcpServers.
// Env values are never exposed — only whether any are set.
type MCPServer struct {
	Name    string `json:"name"`
	Type    string `json:"type"` // http | sse | stdio
	URL     string `json:"url,omitempty"`
	Command string `json:"command,omitempty"` // display form: command + args, no env
	HasEnv  bool   `json:"has_env,omitempty"`

	args   []string
	env    map[string]string
	inline string // raw command when Claude stored a shell line
}

// SkillsDir returns ~/.claude/skills.
func SkillsDir() (string, error) {
	home, err := homeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "skills"), nil
}

// Skills lists the skills in ~/.claude/skills (directories with a SKILL.md),
// sorted by name. A missing directory yields an empty list, not an error.
func Skills() ([]SkillEntry, error) {
	root, err := SkillsDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []SkillEntry
	for _, e := range entries {
		src := filepath.Join(root, e.Name())
		// os.Stat (not DirEntry.IsDir): entries are frequently symlinks into
		// skill managers like ~/.cc-switch/skills and must be followed.
		info, err := os.Stat(src)
		if err != nil || !info.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(src, "SKILL.md"))
		if err != nil {
			continue
		}
		name, desc := parseFrontmatter(e.Name(), data)
		out = append(out, SkillEntry{Name: name, Description: desc, Source: src})
	}
	slices.SortFunc(out, func(a, b SkillEntry) int { return cmp.Compare(a.Name, b.Name) })
	return out, nil
}

// parseFrontmatter extracts name/description from a SKILL.md's YAML
// frontmatter. The directory name stays canonical (the symlink in ~/.woca is
// created by that name); a frontmatter name is honored only when it matches
// the entry's own name's slug rules — mismatched names are ignored so the
// selection always resolves to a real directory.
func parseFrontmatter(defaultName string, data []byte) (name, description string) {
	name = defaultName
	text := strings.TrimSpace(string(data))
	if !strings.HasPrefix(text, "---") {
		return name, ""
	}
	parts := strings.SplitN(text, "---", 3)
	if len(parts) < 3 {
		return name, ""
	}
	for _, line := range strings.Split(parts[1], "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		if strings.TrimSpace(k) == "description" {
			description = strings.TrimSpace(v)
		}
	}
	return name, description
}

// managedLink reports whether path is a symlink this package created (one
// pointing into ~/.claude/skills). Real directories are never managed.
func managedLink(path, claudeRoot string) bool {
	if path == "" || claudeRoot == "" {
		return false
	}
	target, err := os.Readlink(path)
	if err != nil {
		return false
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(path), target)
	}
	target = filepath.Clean(target)
	return strings.HasPrefix(target, filepath.Clean(claudeRoot)+string(filepath.Separator))
}

// SyncSkills symlinks each selected ~/.claude/skills/<name> into
// <wocaRoot>/skills/<name> and removes previously managed links that are no
// longer selected. Real (non-symlink) entries under the woca skills dir are
// never touched. Returns the names now linked.
func SyncSkills(wocaRoot string, selected []string) ([]string, error) {
	claudeRoot, err := SkillsDir()
	if err != nil {
		return nil, err
	}
	destDir := filepath.Join(wocaRoot, "skills")
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, name := range selected {
		src := filepath.Join(claudeRoot, name)
		if _, err := os.Stat(src); err != nil {
			continue // unknown name; skip silently (renamed upstream)
		}
		want[name] = true
	}

	// Drop managed links that are no longer selected.
	existing, err := os.ReadDir(destDir)
	if err != nil {
		return nil, err
	}
	for _, e := range existing {
		p := filepath.Join(destDir, e.Name())
		if !managedLink(p, claudeRoot) {
			continue
		}
		if want[e.Name()] {
			// Refresh stale links (skill moved / re-created upstream).
			_ = os.Remove(p)
		} else {
			if err := os.Remove(p); err != nil {
				return nil, err
			}
		}
	}

	var linked []string
	for name := range want {
		src := filepath.Join(claudeRoot, name)
		dst := filepath.Join(destDir, name)
		if _, err := os.Lstat(dst); err == nil {
			continue // a real entry owns this name; leave it alone
		}
		if err := os.Symlink(src, dst); err != nil {
			return linked, fmt.Errorf("claudeenv: link skill %s: %w", name, err)
		}
		linked = append(linked, name)
	}
	slices.Sort(linked)
	return linked, nil
}

// claudeJSONPath returns ~/.claude.json.
func claudeJSONPath() (string, error) {
	home, err := homeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude.json"), nil
}

// Servers lists the global mcpServers from ~/.claude.json (project-scoped
// servers are intentionally not imported). A missing file yields an empty
// list, not an error.
func Servers() ([]MCPServer, error) {
	path, err := claudeJSONPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var doc struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("claudeenv: decode ~/.claude.json: %w", err)
	}
	out := make([]MCPServer, 0, len(doc.MCPServers))
	for name, raw := range doc.MCPServers {
		srv, err := parseServer(name, raw)
		if err != nil {
			continue // unreadable entry; leave it unlisted
		}
		out = append(out, srv)
	}
	slices.SortFunc(out, func(a, b MCPServer) int { return cmp.Compare(a.Name, b.Name) })
	return out, nil
}

// parseServer decodes one mcpServers entry across the shapes found in
// ~/.claude.json: remote servers carry type http|sse + url; local ones carry
// command + args (+ env), with the type omitted.
func parseServer(name string, raw json.RawMessage) (MCPServer, error) {
	var cfg struct {
		Type    string            `json:"type"`
		URL     string            `json:"url"`
		Command string            `json:"command"`
		Args    []string          `json:"args"`
		Env     map[string]string `json:"env"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return MCPServer{}, err
	}
	srv := MCPServer{
		Name:   name,
		Type:   cfg.Type,
		URL:    cfg.URL,
		HasEnv: len(cfg.Env) > 0,
		args:   cfg.Args,
		env:    cfg.Env,
		inline: strings.TrimSpace(cfg.Command),
	}
	switch {
	case cfg.Type == "http", cfg.Type == "sse":
		if srv.URL == "" {
			return MCPServer{}, fmt.Errorf("server %s: missing url", name)
		}
	case cfg.Type == "stdio", cfg.Type == "" && cfg.Command != "":
		srv.Type = "stdio"
		srv.Command = shellQuote(cfg.Command, cfg.Args)
	default:
		return MCPServer{}, fmt.Errorf("server %s: unsupported shape", name)
	}
	return srv, nil
}

// Spec renders the server as a woca --mcp spec (name=command or name=url).
func (s MCPServer) Spec() string {
	if s.URL != "" {
		return s.Name + "=" + s.URL
	}
	parts := make([]string, 0, len(s.env)+2)
	for _, k := range sortedKeys(s.env) {
		// sh -c prefix assignment: only the value is quoted.
		parts = append(parts, k+"="+shellWord(s.env[k]))
	}
	cmd := s.inline
	if len(s.args) > 0 {
		cmd = shellQuote(cmd, s.args)
	}
	// A stored shell line without args runs as-is under sh -c.
	parts = append(parts, cmd)
	return s.Name + "=" + strings.Join(parts, " ")
}

// WriteMCPConfig materializes the selected servers into <wocaRoot>/mcp.json
// (Claude's {"mcpServers": {…}} shape) so the local copy under ~/.woca is
// the source of truth at agent startup.
func WriteMCPConfig(wocaRoot string, servers []MCPServer) error {
	doc := map[string]any{"mcpServers": map[string]any{}}
	mcp := doc["mcpServers"].(map[string]any)
	for _, s := range servers {
		switch s.Type {
		case "http", "sse":
			mcp[s.Name] = map[string]any{"type": s.Type, "url": s.URL}
		default:
			entry := map[string]any{"type": "stdio", "command": s.inline, "args": s.args}
			if len(s.env) > 0 {
				entry["env"] = s.env
			}
			mcp[s.Name] = entry
		}
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(wocaRoot, 0o755); err != nil {
		return err
	}
	path := filepath.Join(wocaRoot, "mcp.json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// LoadMCPConfig reads the specs previously materialized by WriteMCPConfig
// (the agent-startup half of the handshake).
func LoadMCPConfig(wocaRoot string) ([]string, error) {
	data, err := os.ReadFile(filepath.Join(wocaRoot, "mcp.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var doc struct {
		MCPServers map[string]struct {
			Type    string            `json:"type"`
			URL     string            `json:"url"`
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("claudeenv: decode mcp.json: %w", err)
	}
	names := make([]string, 0, len(doc.MCPServers))
	for name := range doc.MCPServers {
		names = append(names, name)
	}
	slices.Sort(names)
	out := make([]string, 0, len(names))
	for _, name := range names {
		cfg := doc.MCPServers[name]
		srv, err := parseServer(name, mustJSON(cfg))
		if err != nil {
			continue
		}
		out = append(out, srv.Spec())
	}
	return out, nil
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
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
