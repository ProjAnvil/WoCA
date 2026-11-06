package plugins

// Manifest decoding and contribution resolution for the plugin loader
// (plugins.go): the manifest may appear as plugin.json or
// .claude-plugin/plugin.json, and contribution fields are polymorphic —
// a path, a list of paths, or an inline JSON object — across woca and
// Claude spellings. A declared contribution that cannot be resolved is a
// load error: plugins never load partially and silently.

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/projanvil/woca/internal/agents"
	"github.com/projanvil/woca/internal/hooks"
	"github.com/projanvil/woca/internal/mcp"
	"github.com/projanvil/woca/internal/skills"
)

// ignoredManifestKeys are the Claude/Codex manifest contributions woca has
// no receiving surface for this round; their presence is recorded in
// Plugin.Ignored so /plugins can warn instead of silently dropping them.
var ignoredManifestKeys = []string{
	"lspServers", "outputStyles", "workflows", "types", "experimental",
	"apps", "appTemplates", "scheduledTasks", "user_config",
}

// manifest is the decoded plugin manifest: identity fields decoded
// directly, contribution fields kept raw because their shapes are
// polymorphic and normalized by the resolve* functions below. Woca and
// Claude spellings are both accepted; the woca spelling wins when both
// are present.
type manifest struct {
	Name        string
	Version     string
	Description string
	// fields maps every manifest key to its raw JSON value.
	fields map[string]json.RawMessage
}

// readManifest locates and decodes the plugin manifest: plugin.json in
// the plugin root wins over .claude-plugin/plugin.json (first found
// wins).
func readManifest(dir string) (manifest, error) {
	path := filepath.Join(dir, "plugin.json")
	if _, err := os.Stat(path); err != nil {
		path = filepath.Join(dir, ".claude-plugin", "plugin.json")
		if _, err := os.Stat(path); err != nil {
			return manifest{}, fmt.Errorf("plugins: %s: no plugin.json", dir)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return manifest{}, fmt.Errorf("plugins: %s: %w", dir, err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return manifest{}, fmt.Errorf("plugins: parse %s: %w", path, err)
	}
	m := manifest{fields: fields}
	for _, f := range []struct {
		key string
		dst *string
	}{
		{"name", &m.Name},
		{"version", &m.Version},
		{"description", &m.Description},
	} {
		raw, ok := fields[f.key]
		if !ok || string(raw) == "null" {
			continue
		}
		if err := json.Unmarshal(raw, f.dst); err != nil {
			return manifest{}, fmt.Errorf("plugins: parse %s: %s: %w", path, f.key, err)
		}
	}
	return m, nil
}

// field returns the raw value of the first present alias (woca spelling
// first); nil when the field is absent or null.
func (m manifest) field(aliases ...string) json.RawMessage {
	for _, a := range aliases {
		if raw, ok := m.fields[a]; ok && string(raw) != "null" {
			return raw
		}
	}
	return nil
}

// ignored lists the unsupported manifest keys that are present, in
// canonical order.
func (m manifest) ignored() []string {
	var out []string
	for _, k := range ignoredManifestKeys {
		if raw, ok := m.fields[k]; ok && string(raw) != "null" {
			out = append(out, k)
		}
	}
	return out
}

// pathList decodes a path-shaped contribution field: a string or a list
// of strings. Empty entries are dropped; anything else is a broken
// contribution.
func pathList(field string, raw json.RawMessage) ([]string, error) {
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		if one == "" {
			return nil, nil
		}
		return []string{one}, nil
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err == nil {
		return slices.DeleteFunc(many, func(s string) bool { return s == "" }), nil
	}
	return nil, fmt.Errorf("%s: expected path or list of paths", field)
}

// wrapJSON builds {"<key>": <value>} from raw JSON.
func wrapJSON(key string, value json.RawMessage) json.RawMessage {
	b, _ := json.Marshal(map[string]json.RawMessage{key: value})
	return b
}

// contributionDocs resolves a hooks- or mcpServers-shaped manifest field
// into a list of {"<wrapperKey>": {...}} documents:
//   - absent field: defaultPath is read when it exists, else no docs;
//   - string or []string: paths (relative to the plugin dir) of files to
//     read — a declared path that cannot be read is a broken contribution;
//   - inline object: the body directly ({"PreToolUse": ...} event keys or
//     a server-name map), the {"<wrapperKey>": {...}} wrapper form, or a
//     {"source": "<path>"} reference to an external file.
func contributionDocs(field, wrapperKey, defaultPath, dir string, raw json.RawMessage) ([]json.RawMessage, error) {
	if len(raw) == 0 {
		data, err := os.ReadFile(filepath.Join(dir, defaultPath))
		if err != nil {
			if os.IsNotExist(err) {
				return nil, nil
			}
			return nil, fmt.Errorf("%s: %w", field, err)
		}
		return []json.RawMessage{data}, nil
	}
	if paths, err := pathList(field, raw); err == nil {
		var docs []json.RawMessage
		for _, p := range paths {
			data, err := os.ReadFile(filepath.Join(dir, p))
			if err != nil {
				return nil, fmt.Errorf("%s %s: %w", field, p, err)
			}
			docs = append(docs, data)
		}
		return docs, nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("%s: expected path, list of paths or inline object", field)
	}
	if body, ok := obj[wrapperKey]; ok {
		var inner map[string]json.RawMessage
		if err := json.Unmarshal(body, &inner); err != nil {
			return nil, fmt.Errorf("%s: inline object's %q value must be an object", field, wrapperKey)
		}
		return []json.RawMessage{wrapJSON(wrapperKey, body)}, nil
	}
	if src, ok := obj["source"]; ok {
		var p string
		if err := json.Unmarshal(src, &p); err == nil && p != "" {
			data, err := os.ReadFile(filepath.Join(dir, p))
			if err != nil {
				return nil, fmt.Errorf("%s %s: %w", field, p, err)
			}
			return []json.RawMessage{data}, nil
		}
	}
	return []json.RawMessage{wrapJSON(wrapperKey, raw)}, nil
}

// loader resolves one plugin's contributions against its install path.
type loader struct {
	// dir is the plugin install path: the ${...PLUGIN_ROOT} substitution
	// target.
	dir string
}

// resolveContributions fills in p's contribution fields from the
// manifest: skills and agents (dir fields), commands, hooks and MCP
// specs, plus the ignored-keys list.
func resolveContributions(p *Plugin, m manifest, dir string) error {
	l := loader{dir: dir}

	// Skills: skills_dir | skills (string or []string), default "skills".
	skillDirs, declared, err := dirField(m, "skills_dir", "skills", &p.Manifest.SkillsDir)
	if err != nil {
		return err
	}
	if !declared {
		skillDirs = []string{"skills"}
	}
	for _, d := range skillDirs {
		list, ok, err := l.loadSkillDir(d, p.Manifest.Name)
		if err != nil {
			return err
		}
		if !ok && declared {
			return fmt.Errorf("skills %s: no such directory", d)
		}
		p.Skills = append(p.Skills, list...)
	}

	// Agents: agents_dir | agents (string or []string), default "agents".
	agentDirs, declared, err := dirField(m, "agents_dir", "agents", &p.Manifest.AgentsDir)
	if err != nil {
		return err
	}
	if !declared {
		agentDirs = []string{"agents"}
	}
	for _, d := range agentDirs {
		list, ok, err := l.loadAgentDir(d)
		if err != nil {
			return err
		}
		if !ok && declared {
			return fmt.Errorf("agents %s: no such directory", d)
		}
		p.Agents = append(p.Agents, list...)
	}

	// Commands: commands_dir | commands (string or []string), default
	// "commands".
	var cmdDirs []string
	cmdDeclared := false
	if raw := m.field("commands_dir", "commands"); raw != nil {
		cmdDirs, err = pathList("commands_dir", raw)
		if err != nil {
			return err
		}
		cmdDeclared = true
	} else {
		cmdDirs = []string{"commands"}
	}
	for _, d := range cmdDirs {
		list, ok, err := l.loadCommandDir(d, p.Manifest.Name)
		if err != nil {
			return err
		}
		if !ok && cmdDeclared {
			return fmt.Errorf("commands %s: no such directory", d)
		}
		p.Commands = append(p.Commands, list...)
	}

	// Hooks: hooks file (default hooks/hooks.json), path list or inline
	// object, with ${...PLUGIN_ROOT} substituted into hook commands.
	docs, err := contributionDocs("hooks", "hooks", filepath.Join("hooks", "hooks.json"), dir, m.field("hooks"))
	if err != nil {
		return err
	}
	var hookCfg hooks.Config
	for _, doc := range docs {
		c, err := hooks.Parse(doc)
		if err != nil {
			return fmt.Errorf("hooks: %w", err)
		}
		hookCfg = mergeHookConfigs(hookCfg, c)
	}
	tagAndSubstHooks(&hookCfg, p.Manifest.Name, l.substRoot)
	p.Hooks = hookCfg

	// MCP servers: .mcp.json (default), declared path list or inline
	// mcpServers object; each entry becomes a woca --mcp spec.
	mcpDocs, err := contributionDocs("mcp_servers", "mcpServers", ".mcp.json", dir, m.field("mcp_servers", "mcpServers"))
	if err != nil {
		return err
	}
	servers := map[string]json.RawMessage{}
	for _, doc := range mcpDocs {
		var wrapper struct {
			MCPServers map[string]json.RawMessage `json:"mcpServers"`
		}
		if err := json.Unmarshal(doc, &wrapper); err != nil {
			return fmt.Errorf("mcpServers: %w", err)
		}
		maps.Copy(servers, wrapper.MCPServers) // later documents win on name conflicts
	}
	for _, name := range slices.Sorted(maps.Keys(servers)) {
		entry, err := mcp.ParseServerEntry(name, servers[name])
		if err != nil {
			return fmt.Errorf("mcpServers: %w", err)
		}
		p.MCPSpecs = append(p.MCPSpecs, l.substRoot(entry.Spec()))
	}

	p.Ignored = m.ignored()
	return nil
}

// dirField resolves a directory-shaped field (string or list of strings),
// recording the first declared dir into dst for display compatibility.
// declared reports whether the field was present at all.
func dirField(m manifest, woca, claude string, dst *string) (dirs []string, declared bool, err error) {
	raw := m.field(woca, claude)
	if raw == nil {
		return nil, false, nil
	}
	dirs, err = pathList(woca, raw)
	if err != nil {
		return nil, true, err
	}
	if len(dirs) > 0 {
		*dst = dirs[0]
	}
	return dirs, true, nil
}

// loadSkillDir loads one skills directory, namespacing skill names with
// the plugin name. ok=false when the directory does not exist.
func (l loader) loadSkillDir(d, plugin string) (list []skills.Skill, ok bool, err error) {
	path := filepath.Join(l.dir, d)
	st, statErr := os.Stat(path)
	if statErr != nil || !st.IsDir() {
		return nil, false, nil
	}
	list, err = skills.Load(path)
	if err != nil {
		return nil, true, fmt.Errorf("skills %s: %w", d, err)
	}
	for i := range list {
		list[i].Name = plugin + "-" + list[i].Name // namespace plugin skills
	}
	return list, true, nil
}

// loadAgentDir loads one agents directory. ok=false when the directory
// does not exist.
func (l loader) loadAgentDir(d string) (list []agents.Definition, ok bool, err error) {
	path := filepath.Join(l.dir, d)
	st, statErr := os.Stat(path)
	if statErr != nil || !st.IsDir() {
		return nil, false, nil
	}
	list, err = agents.Load(path)
	if err != nil {
		return nil, true, fmt.Errorf("agents %s: %w", d, err)
	}
	return list, true, nil
}

// loadCommandDir loads one commands directory: every *.md file becomes a
// skills.Skill named "<plugin>:<stem>" with Claude's $ARGUMENTS/$N
// placeholders rewritten to woca's {arguments}/{argN}. ok=false when the
// directory does not exist.
func (l loader) loadCommandDir(d, plugin string) ([]skills.Skill, bool, error) {
	path := filepath.Join(l.dir, d)
	st, err := os.Stat(path)
	if err != nil || !st.IsDir() {
		return nil, false, nil
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, true, err
	}
	var out []skills.Skill
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		file := filepath.Join(path, e.Name())
		data, err := os.ReadFile(file)
		if err != nil {
			return nil, true, err
		}
		stem := strings.TrimSuffix(e.Name(), ".md")
		sk := skills.Parse(plugin+":"+stem, data)
		sk.Name = plugin + ":" + stem // deterministic; frontmatter cannot rename
		rewriteCommandPlaceholders(&sk)
		sk.Source = file
		out = append(out, sk)
	}
	return out, true, nil
}

// rewriteCommandPlaceholders maps Claude's $ARGUMENTS and $1..$9 to
// woca's {arguments}/{arg1}..{arg9} and declares the placeholders
// actually present as skill arguments.
func rewriteCommandPlaceholders(sk *skills.Skill) {
	declare := func(name string) {
		for _, a := range sk.Args {
			if a.Name == name {
				return
			}
		}
		sk.Args = append(sk.Args, skills.SkillArg{Name: name})
	}
	if strings.Contains(sk.Body, "$ARGUMENTS") {
		sk.Body = strings.ReplaceAll(sk.Body, "$ARGUMENTS", "{arguments}")
		declare("arguments")
	}
	for i := range 9 {
		n := strconv.Itoa(i + 1)
		if strings.Contains(sk.Body, "$"+n) {
			sk.Body = strings.ReplaceAll(sk.Body, "$"+n, "{arg"+n+"}")
			declare("arg" + n)
		}
	}
}

// substRoot replaces ${CLAUDE_PLUGIN_ROOT} and ${WOCA_PLUGIN_ROOT} with
// the plugin's install path (plain text; no shell evaluation).
func (l loader) substRoot(s string) string {
	s = strings.ReplaceAll(s, "${CLAUDE_PLUGIN_ROOT}", l.dir)
	return strings.ReplaceAll(s, "${WOCA_PLUGIN_ROOT}", l.dir)
}

// mergeHookConfigs appends b's entries to a's: multiple hooks documents
// from one plugin run together.
func mergeHookConfigs(a, b hooks.Config) hooks.Config {
	appendList := func(dst *[]hooks.Entry, src []hooks.Entry) { *dst = append(*dst, src...) }
	appendList(&a.PreToolUse, b.PreToolUse)
	appendList(&a.PostToolUse, b.PostToolUse)
	appendList(&a.UserPromptSubmit, b.UserPromptSubmit)
	appendList(&a.Stop, b.Stop)
	appendList(&a.SessionStart, b.SessionStart)
	appendList(&a.SessionEnd, b.SessionEnd)
	appendList(&a.SubagentStart, b.SubagentStart)
	appendList(&a.SubagentStop, b.SubagentStop)
	appendList(&a.Notification, b.Notification)
	appendList(&a.PreCompact, b.PreCompact)
	appendList(&a.PostCompact, b.PostCompact)
	appendList(&a.PermissionRequest, b.PermissionRequest)
	appendList(&a.PostSampling, b.PostSampling)
	return a
}

// tagAndSubstHooks walks every event entry of a plugin's hooks config:
// entries are tagged with their Source for /hooks display, and
// ${...PLUGIN_ROOT} placeholders in hook commands are substituted with
// the install path.
func tagAndSubstHooks(cfg *hooks.Config, source string, subst func(string) string) {
	events := []*[]hooks.Entry{
		&cfg.PreToolUse, &cfg.PostToolUse, &cfg.UserPromptSubmit, &cfg.Stop,
		&cfg.SessionStart, &cfg.SessionEnd, &cfg.SubagentStart, &cfg.SubagentStop,
		&cfg.Notification, &cfg.PreCompact, &cfg.PostCompact, &cfg.PermissionRequest,
		&cfg.PostSampling,
	}
	for _, entries := range events {
		for i := range *entries {
			e := &(*entries)[i]
			if e.Source == "" {
				e.Source = source
			}
			for j := range e.Hooks {
				e.Hooks[j].Command = subst(e.Hooks[j].Command)
			}
		}
	}
}
