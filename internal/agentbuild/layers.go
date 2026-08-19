package agentbuild

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/rs/zerolog"

	coretools "github.com/projanvil/langchain-golang/core/tools"

	"github.com/projanvil/woca/internal/agents"
	"github.com/projanvil/woca/internal/hooks"
	"github.com/projanvil/woca/internal/mcp"
	"github.com/projanvil/woca/internal/merge"
	"github.com/projanvil/woca/internal/permissions"
	"github.com/projanvil/woca/internal/platform/executor"
	"github.com/projanvil/woca/internal/settings"
	"github.com/projanvil/woca/internal/skills"
)

// loadHooksConfig merges the user (~/.woca/settings.json) and project
// (.woca/settings.json) hook configs.
func loadHooksConfig(workdir string) hooks.Config {
	paths := []string{}
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, ".woca", "settings.json"))
	}
	paths = append(paths, filepath.Join(workdir, ".woca", "settings.json"))

	var merged hooks.Config
	for _, p := range paths {
		cfg, err := hooks.Load(p)
		if err != nil {
			continue
		}
		merged.PreToolUse = append(merged.PreToolUse, cfg.PreToolUse...)
		merged.PostToolUse = append(merged.PostToolUse, cfg.PostToolUse...)
		merged.UserPromptSubmit = append(merged.UserPromptSubmit, cfg.UserPromptSubmit...)
		merged.Stop = append(merged.Stop, cfg.Stop...)
		merged.SessionStart = append(merged.SessionStart, cfg.SessionStart...)
		merged.SessionEnd = append(merged.SessionEnd, cfg.SessionEnd...)
		merged.SubagentStop = append(merged.SubagentStop, cfg.SubagentStop...)
		merged.Notification = append(merged.Notification, cfg.Notification...)
		merged.PreCompact = append(merged.PreCompact, cfg.PreCompact...)
		merged.PostCompact = append(merged.PostCompact, cfg.PostCompact...)
		merged.PermissionRequest = append(merged.PermissionRequest, cfg.PermissionRequest...)
		merged.PostSampling = append(merged.PostSampling, cfg.PostSampling...)
	}
	return merged
}

// loadPermissions loads permission rules from settings.json files (user and
// project), merging them.
func loadPermissions(workdir string) *permissions.Rules {
	paths := []string{}
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, ".woca", "settings.json"))
	}
	paths = append(paths, filepath.Join(workdir, ".woca", "settings.json"))

	var merged permissions.Rules
	for _, p := range paths {
		r, err := permissions.Load(p)
		if err != nil {
			continue
		}
		merged.Allow = append(merged.Allow, r.Allow...)
		merged.Deny = append(merged.Deny, r.Deny...)
		merged.Ask = append(merged.Ask, r.Ask...)
	}
	return &merged
}

// loadSkills loads skills from layered directories: workspace roots (workDir
// up to the git root, .woca/skills before .claude/skills in each directory)
// win over the user root ~/.woca/skills. Same-name skills resolve to the
// higher layer — workspace beats user, which reverses the old first-hit
// semantics where the user root (loaded first) silently won.
func loadSkills(workdir string, log zerolog.Logger) []skills.Skill {
	var es []merge.Entry[skills.Skill]
	for i, dir := range merge.WorkspaceDirs(workdir) {
		for j, sub := range []string{".woca", ".claude"} {
			root := filepath.Join(dir, sub, "skills")
			list, err := skills.Load(root)
			if err != nil || len(list) == 0 {
				continue
			}
			for _, s := range list {
				es = append(es, merge.Entry[skills.Skill]{
					Name: s.Name, Value: s, Level: merge.LevelWorkspace,
					Order: i*2 + j, Source: s.Source,
				})
			}
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		if list, err := skills.Load(filepath.Join(home, ".woca", "skills")); err == nil {
			for _, s := range list {
				es = append(es, merge.Entry[skills.Skill]{
					Name: s.Name, Value: s, Level: merge.LevelUser,
					Order: 0, Source: s.Source,
				})
			}
		}
	}
	merged, conflicts := merge.Entries(es)
	logMergeConflicts(log, "skill", conflicts)
	out := make([]skills.Skill, 0, len(merged))
	for _, e := range merged {
		out = append(out, e.Value)
	}
	return out
}

// logMergeConflicts emits one structured log line per layered-merge
// conflict (D6: observability without a UI panel).
func logMergeConflicts(log zerolog.Logger, kind string, conflicts []merge.Conflict) {
	for _, c := range conflicts {
		log.Info().Str("kind", kind).Str("name", c.Name).
			Str("winner", c.Winner).Str("loser", c.Loser).
			Msg("config merge conflict resolved")
	}
}

// skillManifest renders the skill list injected into the system prompt (the
// Skill tool's static description no longer carries it — the list is
// workspace-scoped).
func skillManifest(list []skills.Skill) string {
	var b strings.Builder
	for _, s := range list {
		fmt.Fprintf(&b, "- %s: %s\n", s.Name, s.Description)
	}
	return b.String()
}

// loadAgents loads custom subagent definitions from layered directories:
// workDir/.woca/agents plus every ancestor's up to the git root, winning
// over the user root ~/.woca/agents on name conflicts.
func loadAgents(workdir string, log zerolog.Logger) []agents.Definition {
	var es []merge.Entry[agents.Definition]
	for i, dir := range merge.WorkspaceDirs(workdir) {
		list, err := agents.Load(filepath.Join(dir, ".woca", "agents"))
		if err != nil || len(list) == 0 {
			continue
		}
		for _, d := range list {
			es = append(es, merge.Entry[agents.Definition]{
				Name: d.Name, Value: d, Level: merge.LevelWorkspace,
				Order: i, Source: d.Source,
			})
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		if list, err := agents.Load(filepath.Join(home, ".woca", "agents")); err == nil {
			for _, d := range list {
				es = append(es, merge.Entry[agents.Definition]{
					Name: d.Name, Value: d, Level: merge.LevelUser,
					Order: 0, Source: d.Source,
				})
			}
		}
	}
	merged, conflicts := merge.Entries(es)
	logMergeConflicts(log, "agent", conflicts)
	out := make([]agents.Definition, 0, len(merged))
	for _, e := range merged {
		out = append(out, e.Value)
	}
	return out
}

// mergeMCPSpecs deduplicates name=command MCP specs across the three layers:
// --mcp flags beat workDir/.woca/mcp.json beat ~/.woca/mcp.json (D5/D6).
// Same-layer duplicates keep the first occurrence; invalid specs are warned
// and dropped. Connecting the merged list fixes the same-name
// double-connect bug.
func mergeMCPSpecs(flag, workspace, user []string, log zerolog.Logger) []string {
	layer := func(specs []string, lvl merge.Level) []merge.Entry[string] {
		var es []merge.Entry[string]
		for i, spec := range specs {
			name, _, ok := strings.Cut(spec, "=")
			if !ok || strings.TrimSpace(name) == "" {
				log.Warn().Str("spec", spec).Str("layer", lvl.String()).Msg("invalid mcp spec, expected name=command")
				continue
			}
			es = append(es, merge.Entry[string]{
				Name: name, Value: spec, Level: lvl, Order: i,
				Source: lvl.String(),
			})
		}
		return es
	}
	es := append(append(layer(flag, merge.LevelFlag), layer(workspace, merge.LevelWorkspace)...), layer(user, merge.LevelUser)...)
	merged, conflicts := merge.Entries(es)
	logMergeConflicts(log, "mcp", conflicts)
	out := make([]string, 0, len(merged))
	for _, e := range merged {
		out = append(out, e.Value)
	}
	return out
}

// connectMCP starts MCP servers and returns their tools plus the clients
// (kept for between-turn tool refreshes). Server processes spawn through x.
func connectMCP(ctx context.Context, log zerolog.Logger, servers []string, st *settings.Store, x executor.Executor) ([]coretools.Tool, []*mcp.Client) {
	var out []coretools.Tool
	var clients []*mcp.Client
	for _, spec := range servers {
		name, command, ok := strings.Cut(spec, "=")
		if !ok || strings.TrimSpace(command) == "" {
			log.Warn().Str("spec", spec).Msg("invalid --mcp value, expected name=command or name=http(s)://endpoint")
			continue
		}
		var client *mcp.Client
		if strings.HasPrefix(command, "http://") || strings.HasPrefix(command, "https://") {
			// OAuth-authorized servers carry their stored token (encrypted
			// in the secrets table by `woca mcp auth` / the McpAuth tool).
			var token func() string
			if st != nil {
				secret := mcp.McpTokenSecretPrefix + name + "_token"
				if st.HasSecret(secret) {
					token = func() string { return st.GetSecret(secret) }
				}
			}
			client = mcp.NewStreamableHTTPAuth(name, command, token)
		} else {
			client = mcp.NewStdioWithExecutor(x, name, "sh", "-c", command)
		}
		if client == nil {
			log.Warn().Str("name", name).Msg("failed to start MCP server")
			continue
		}
		if err := client.Initialize(ctx); err != nil {
			log.Warn().Str("name", name).Err(err).Msg("MCP initialize failed")
			continue
		}
		list, err := client.ListTools(ctx)
		if err != nil {
			log.Warn().Str("name", name).Err(err).Msg("MCP tools/list failed")
			continue
		}
		clients = append(clients, client)
		out = append(out, client.AsTools(name, list)...)
		out = append(out, client.AsResourceTools(name)...)
		out = append(out, client.AsPromptTools(name)...)
		log.Info().Str("name", name).Int("tools", len(list)).Msg("connected MCP server")
	}
	return out, clients
}

// refreshMCPTools re-lists tools from connected MCP servers, for the
// between-turn tool refresh (hot-added tools become usable next round).
func refreshMCPTools(ctx context.Context, log zerolog.Logger, clients []*mcp.Client) []coretools.Tool {
	var out []coretools.Tool
	for _, client := range clients {
		list, err := client.ListTools(ctx)
		if err != nil {
			continue
		}
		name := client.Name()
		out = append(out, client.AsTools(name, list)...)
		out = append(out, client.AsResourceTools(name)...)
		out = append(out, client.AsPromptTools(name)...)
	}
	return out
}
