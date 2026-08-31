// Package slashcmds holds the builtin slash command table. It lives apart
// from internal/console so the web server (which console transitively
// depends on) can share the same list for its autocomplete API without an
// import cycle.
package slashcmds

import "sort"

// Command describes one slash command for help and completion.
type Command struct {
	Name        string
	Description string
}

// Builtin returns woca's builtin slash commands, sorted by name. These are
// the same commands the TUI console executes.
func Builtin() []Command {
	list := []Command{
		{Name: "help", Description: "list available commands"},
		{Name: "status", Description: "show version, model, mode, and session stats"},
		{Name: "config", Description: "alias for /status"},
		{Name: "permissions", Description: "show or set the permission mode"},
		{Name: "thinking", Description: "show or set the thinking level (off | low | medium | high | tokens)"},
		{Name: "quota", Description: "show the plan's remaining quota (GLM Coding Plan)"},
		{Name: "model", Description: "show or switch the model"},
		{Name: "clear", Description: "clear the conversation and start fresh"},
		{Name: "compact", Description: "summarize the conversation to free context"},
		{Name: "context", Description: "show context window usage"},
		{Name: "cost", Description: "show cumulative token usage and estimated cost"},
		{Name: "init", Description: "analyze the codebase and generate an AGENTS.md project guide"},
		{Name: "rewind", Description: "rewind the conversation to an earlier point"},
		{Name: "branch", Description: "branch off a copy of the current session"},
		{Name: "resume", Description: "list or load a saved session snapshot (/resume [name])"},
		{Name: "changes", Description: "show pending workspace changes (preview before /undo)"},
		{Name: "doctor", Description: "check config, credentials, storage, hooks, and permissions"},
		{Name: "export", Description: "export the conversation as markdown or resumable JSON"},
		{Name: "rename", Description: "name the current session snapshot for later --resume"},
		{Name: "agents", Description: "manage subagent definitions (list | show | new | rm)"},
		{Name: "output-style", Description: "show or set the output style (default | explanatory | learning)"},
		{Name: "plugins", Description: "manage plugins (list | install | rm)"},
		{Name: "theme", Description: "show or set the color theme (dark | light | high-contrast)"},
		{Name: "vim", Description: "toggle vim modal input editing"},
		{Name: "keybindings", Description: "show the keybinding reference"},
		{Name: "todos", Description: "show the current todo list"},
		{Name: "undo", Description: "revert the last file change made by Write/Edit (repeatable; Bash changes are not tracked)"},
		{Name: "redo", Description: "re-apply the most recently undone file change"},
		{Name: "goal", Description: "autonomous goal loop (<objective> | set | status | pause | resume | budget | clear)"},
		{Name: "tasks", Description: "show the shared task list"},
		{Name: "team", Description: "show the agent team (teammates, shared tasks, inbox)"},
		{Name: "skills", Description: "list loaded skills"},
		{Name: "mcp", Description: "list connected MCP servers and tools"},
		{Name: "settings", Description: "show or update persisted settings (provider, keys, storage backends)"},
		{Name: "debug-log", Description: "toggle harness observability logging to ~/.woca/logs (on | off)"},
		{Name: "memory", Description: "factual memory: recall/remember/stats/forget/consolidate/index + legacy kv store"},
		{Name: "web", Description: "start/stop the web GUI"},
		{Name: "exit", Description: "exit woca"},
		{Name: "quit", Description: "exit woca"},
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	return list
}
