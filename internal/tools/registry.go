// Package tools implements the Claude-Code-style tool set for the woca
// harness: shell execution, file read/write/edit, glob/grep search, a todo
// list, and web access. Each tool is a langchain-golang core/tools.Tool built
// with FromFunc, so they plug directly into a language.ChatModel.BindTools.
package tools

import (
	"context"
	"fmt"
	"sort"

	coretools "github.com/projanvil/langchain-golang/core/tools"
)

// Category classifies a tool for the permission system.
type Category int

const (
	CatRead  Category = iota // read-only filesystem inspection
	CatWrite                 // mutates the filesystem
	CatShell                 // executes shell commands
	CatWeb                   // network access
	CatMeta                  // harness state (todo list)
)

// ReadOnly reports whether the category never mutates external state.
func (c Category) ReadOnly() bool { return c == CatRead || c == CatWeb }

// Safe reports whether the category can run without prompting in default mode.
func (c Category) Safe() bool { return c == CatRead || c == CatWeb || c == CatMeta }

var categoryByName = map[string]Category{
	"Bash":            CatShell,
	"BashOutput":      CatRead,
	"KillShell":       CatMeta,
	"Read":            CatRead,
	"Write":           CatWrite,
	"Edit":            CatWrite,
	"Glob":            CatRead,
	"Grep":            CatRead,
	"ListDir":         CatRead,
	"TodoWrite":       CatMeta,
	"WebFetch":        CatWeb,
	"WebSearch":       CatWeb,
	"AskUserQuestion": CatMeta,
	"EnterPlanMode":   CatMeta,
	"ExitPlanMode":    CatMeta,
	"Task":            CatMeta,
	"TaskOutput":      CatMeta,
	"TaskStop":        CatMeta,
	"TaskCreate":      CatMeta,
	"TaskList":        CatMeta,
	"TaskGet":         CatMeta,
	"TaskUpdate":      CatMeta,
	"ScheduleCron":    CatMeta,
	"GetGoal":         CatMeta,
	"UpdateGoal":      CatMeta,
	"McpAuth":         CatMeta,
	"EnterWorktree":   CatShell,
	"ExitWorktree":    CatShell,
	"LSP":             CatRead,
	"recall_memory":   CatRead,
	"search_code":     CatRead,
	"remember_fact":   CatMeta,  // one row into the local memory DB (TodoWrite class)
	"index_code":      CatWrite, // cost consent gate: walks and indexes a source tree
	// Built-in capability plugins (woca-plugins/; see internal/pluginrt).
	// Browsing pages is read-only web access; acting on them (click/fill —
	// submits forms, changes state) is a write. PDF reads are read-only.
	"BrowserNavigate": CatWeb,
	"BrowserSnapshot": CatWeb,
	"BrowserText":     CatWeb,
	"BrowserAction":   CatWrite,
	"PdfRead":         CatRead,
	"PdfWrite":        CatWrite,
}

// CategoryOf returns the permission category of a tool by name. Unknown tools
// are treated as shell (most sensitive).
func CategoryOf(name string) Category {
	if c, ok := categoryByName[name]; ok {
		return c
	}
	return CatShell
}

// IsReadOnly reports whether a tool invocation only reads state. It is
// input-sensitive: Bash is read-only when its command is a read-only command.
func IsReadOnly(name string, args map[string]any) bool {
	switch CategoryOf(name) {
	case CatRead, CatWeb, CatMeta:
		return true
	case CatWrite:
		return false
	default: // CatShell
		if name == "Bash" {
			if command, ok := args["command"].(string); ok {
				return IsReadOnlyCommand(command)
			}
		}
		return false
	}
}

// Registry is an ordered, name-indexed collection of tools.
type Registry struct {
	byName  map[string]coretools.Tool
	order   []string
	todos   *TodoStore
	dir     *Dir
	bg      *BackgroundShells
	history *FileHistory
	lspPool *lspPool
}

// NewRegistry builds a registry populated with the default tool set, bound to
// the given working directory.
func NewRegistry(workdir string) *Registry {
	if workdir == "" {
		workdir = "."
	}
	r := &Registry{
		byName:  map[string]coretools.Tool{},
		todos:   NewTodoStore(),
		dir:     NewDir(workdir),
		bg:      NewBackgroundShells(),
		history: NewFileHistory(),
		lspPool: newLSPPool(func(cmd []string) (lspTransport, error) { return startLSP(cmd) }),
	}
	for _, t := range defaultTools(r.dir, r.todos, r.bg, r.history, r.lspPool) {
		r.Register(t)
	}
	return r
}

// Close shuts down pooled LSP servers. Safe to call multiple times.
func (r *Registry) Close() {
	if r.lspPool != nil {
		r.lspPool.Close()
	}
}

// LSPStats reports pooled/broken LSP server counts (for /api/stats).
func (r *Registry) LSPStats() (active, broken int) {
	if r.lspPool == nil {
		return 0, 0
	}
	return r.lspPool.stats()
}

// FileHistory returns the file-level undo/redo log shared by the mutating
// file tools (backs /undo and /redo).
func (r *Registry) FileHistory() *FileHistory { return r.history }

// Background returns the manager for run_in_background Bash commands,
// BashOutput, and KillShell.
func (r *Registry) Background() *BackgroundShells { return r.bg }

// Dir returns the session's mutable working directory (worktree switching).
func (r *Registry) Dir() *Dir { return r.dir }

// Register adds (or replaces) a tool.
func (r *Registry) Register(t coretools.Tool) {
	if t == nil {
		return
	}
	if _, ok := r.byName[t.Name()]; !ok {
		r.order = append(r.order, t.Name())
	}
	r.byName[t.Name()] = t
}

// Get returns a tool by name.
func (r *Registry) Get(name string) (coretools.Tool, bool) {
	t, ok := r.byName[name]
	return t, ok
}

// All returns the tools in registration order.
func (r *Registry) All() []coretools.Tool {
	out := make([]coretools.Tool, 0, len(r.order))
	for _, name := range r.order {
		out = append(out, r.byName[name])
	}
	return out
}

// TodoStore returns the shared todo store used by TodoWrite.
func (r *Registry) TodoStore() *TodoStore { return r.todos }

// Execute runs a single tool invocation. Tool-level errors are converted into
// the result content (mirroring langgraph's ToolNode behavior) so the model
// can observe and recover from them; it does not return an error except for
// infrastructure failures.
func (r *Registry) Execute(ctx context.Context, name string, args map[string]any) coretools.Result {
	t, ok := r.byName[name]
	if !ok {
		return coretools.Result{Content: fmt.Sprintf("Error: unknown tool %q", name)}
	}
	res, err := t.Invoke(ctx, args)
	if err != nil {
		return coretools.Result{Content: fmt.Sprintf("Error: %v", err)}
	}
	return res
}

// sortedEntries is a small helper for deterministic directory listings.
func sortedEntries(entries []string) []string {
	sort.Strings(entries)
	return entries
}
