// Package agent builds the woca coding agent as an explicit langgraph-style
// graph: a model node bound to the tool set, a tools node that executes tool
// calls (with a permission gate and user-interaction interrupts), and a
// conditional edge that loops back through the tools until the model stops
// requesting them. It also provides plan mode and Task-spawned subagents.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"
	coretools "github.com/projanvil/langchain-golang/core/tools"
	"golang.org/x/sync/singleflight"

	"github.com/projanvil/woca/internal/agents"
	"github.com/projanvil/woca/internal/bus"
	"github.com/projanvil/woca/internal/compact"
	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/graph"
	"github.com/projanvil/woca/internal/hooks"
	"github.com/projanvil/woca/internal/memory"
	"github.com/projanvil/woca/internal/model"
	"github.com/projanvil/woca/internal/obslog"
	"github.com/projanvil/woca/internal/permissions"
	"github.com/projanvil/woca/internal/prompt"
	"github.com/projanvil/woca/internal/runws"
	"github.com/projanvil/woca/internal/settings"
	"github.com/projanvil/woca/internal/skills"
	"github.com/projanvil/woca/internal/store"
	"github.com/projanvil/woca/internal/tools"
)

const (
	nodeModel = "model"
	nodeTools = "tools"
)

// MessagesReducer implements langgraph's add_messages semantics: append a
// batch of messages, replacing any existing message with the same ID.
func MessagesReducer(existing, update any) any {
	var base []messages.Message
	if existing != nil {
		base, _ = existing.([]messages.Message)
	}
	add, _ := update.([]messages.Message)

	byID := map[string]int{}
	for i, m := range base {
		if m.ID != "" {
			byID[m.ID] = i
		}
	}
	out := append([]messages.Message(nil), base...)
	for _, m := range add {
		if m.ID != "" {
			if idx, ok := byID[m.ID]; ok {
				out[idx] = m
				continue
			}
			byID[m.ID] = len(out)
		}
		out = append(out, m)
	}
	return out
}

// MessagesFrom extracts the message slice from a graph state.
func MessagesFrom(state graph.State) []messages.Message {
	if state == nil {
		return nil
	}
	msgs, _ := state["messages"].([]messages.Message)
	return msgs
}

// PermissionRequest is surfaced when a tool needs approval.
type PermissionRequest struct {
	ToolName   string         `json:"tool_name"`
	ToolArgs   map[string]any `json:"tool_args"`
	Reason     string         `json:"reason"`
	IsReadOnly bool           `json:"is_read_only"`
}

// AskUserRequest is surfaced by the AskUserQuestion tool.
type AskUserRequest struct {
	Questions []tools.Question `json:"questions"`
}

// AskUserAnswer is the resume value for AskUserRequest: one answer per question.
type AskUserAnswer struct {
	Answers []string `json:"answers"`
}

// PlanApprovalRequest is surfaced by ExitPlanMode.
type PlanApprovalRequest struct {
	Plan string `json:"plan"`
}

// Config configures an Agent.
type Config struct {
	Model          language.ChatModel
	Registry       *tools.Registry
	SystemPrompt   string
	PermissionMode config.PermissionMode
	RecursionLimit int
	WorkDir        string
	// SystemPromptFor, when set, builds the system prompt for every run from
	// its workspace and the active model name (serve-mode sessions in other
	// projects need the environment section, git status and project memory
	// files to name the session's directory; the model name appears in the
	// prompt and must track SetModel). Results are cached per
	// workspace+model. Nil keeps SystemPrompt for every run.
	SystemPromptFor func(workDir, model string) string
	// SkillsFor, when set, resolves the skill set for a run's workspace
	// (project-level .woca/skills and .claude/skills must follow the session
	// workspace). Nil keeps Skills.
	SkillsFor func(workdir string) []skills.Skill
	// AgentsFor, when set, resolves custom subagent definitions for a run's
	// workspace. Nil keeps Agents.
	AgentsFor func(workdir string) []agents.Definition
	// HooksFor, when set, builds the hook runner for a run's workspace
	// (project-level hooks config and the hooks' cwd follow the session
	// workspace). Nil keeps Hooks.
	HooksFor func(workdir string) *hooks.Runner
	// Subagent marks this agent as a Task-spawned subagent: it excludes the
	// Task/AskUserQuestion/plan-mode tools.
	Subagent bool
	// SubagentModel optionally overrides the model used by Task-spawned
	// subagents (defaults to Model).
	SubagentModel language.ChatModel
	// Hooks optionally runs PreToolUse/PostToolUse/Stop hooks around tool calls.
	Hooks *hooks.Runner
	// Skills are loaded skills exposed via the Skill tool.
	Skills []skills.Skill
	// ExtraTools are appended to the tool set (e.g. MCP tools).
	ExtraTools []coretools.Tool
	// CompactThreshold enables auto-compaction above this token estimate
	// (0 disables).
	CompactThreshold int
	// Summarizer is the model used to summarize during auto-compaction.
	Summarizer language.ChatModel
	// Bus, when set, enables the SendMessage tool (team coordination).
	Bus *bus.Bus
	// Permissions are explicit allow/deny/ask rules, checked before the mode.
	Permissions *permissions.Rules
	// FallbackModel, when set, is used by a model.Router after retries fail.
	FallbackModel language.ChatModel
	// ModelMaxRetries configures transient-error retries (0 = default 2).
	ModelMaxRetries int
	// Agents are custom subagent definitions for the Task tool.
	Agents []agents.Definition
	// CheckpointDir, when set, persists graph checkpoints for cross-process
	// interrupt/resume (file backend).
	CheckpointDir string
	// Checkpointer, when set, is used directly as the graph checkpointer
	// (e.g. the sqlite/postgres backend selected in settings). It takes
	// precedence over CheckpointDir.
	Checkpointer graph.Checkpointer
	// MaxTokensEscalator, when set, rebuilds the chat model with a higher
	// output-token cap for the escalation path of max-output-tokens recovery
	// (the 8k→64k retry). Wire it only when the user did not set an explicit
	// cap; nil disables escalation and recovery goes straight to the
	// continuation-message path.
	MaxTokensEscalator func() (language.ChatModel, error)
	// ToolResultBudgetChars is the aggregate character budget per tool round
	// applied when assembling model requests (0 = the 200k default; negative
	// disables).
	ToolResultBudgetChars int
	// EmitToolUseSummaries generates short human-readable labels for each
	// completed tool batch asynchronously (UI-only, never enters the model
	// context). Requires Summarizer; skipped for subagents. Wire it for
	// interactive sessions only, mirroring Claude Code's mobile-display gate.
	EmitToolUseSummaries bool
	// RefreshTools, when set, re-collects the extra (non-registry) tools
	// between turns (Claude Code's refreshTools): a changed set rebinds the
	// model so newly available tools (e.g. hot-added MCP tools) are usable
	// in the next round.
	RefreshTools func(ctx context.Context) []coretools.Tool
	// Memory is the long-term memoryStore; when set, relevant memories are
	// prefetched at turn start and attached after tool rounds (Claude Code's
	// memory attachments).
	Memory *store.Store
	// MemoryEngine is the factual-memory engine (ladyM). When set, the
	// per-turn prefetch runs semantic Recall instead of the KV keyword
	// match. Nil disables the subsystem.
	MemoryEngine memory.Engine
	// LoadSettings re-reads persisted settings per run (nil-safe to omit in
	// tests); the memory paths read ladym-topk / ladym-autoretain /
	// ladym-system2 live.
	LoadSettings func() settings.Settings
	// MaxTurns caps the model rounds within one turn (Claude Code's SDK
	// maxTurns); 0 is unlimited. Reaching the cap ends the turn with a
	// <max-turns-reached> marker.
	MaxTurns int
	// BashPolicy, when non-empty, gates Bash commands (allow/deny).
	BashPolicy tools.CommandPolicy
	// McpAuth, when set, runs the OAuth flow for an MCP server (the McpAuth
	// tool); nil omits the tool.
	McpAuth func(ctx context.Context, server string) error
	// Obs is the observability log (graph/state/session/message tracing).
	// Nil disables recording; the store's own enabled flag gates every write.
	Obs *obslog.Store
}

// Agent is the compiled coding-agent graph plus its runtime dependencies.
type Agent struct {
	model          language.ChatModel
	boundModel     language.ChatModel
	registry       *tools.Registry
	toolList       []coretools.Tool
	toolByName     map[string]coretools.Tool
	systemPrompt   string
	mode           config.PermissionMode
	workdir        string
	recursionLimit int
	subagentModel  language.ChatModel
	graph          *graph.CompiledGraph
	tasks          *TaskManager
	taskList       *TaskList
	hooks          *hooks.Runner
	skills         []skills.Skill
	compactLimit   int
	summarizer     language.ChatModel
	bus            *bus.Bus
	// agentName is this session's identity in a team (lead for the
	// interactive session, the teammate name for spawned members).
	agentName string
	// team is the lazily-created agent team (nil until TeamSpawn).
	team       *TeamManager
	perms      *permissions.Rules
	agentDefs  []agents.Definition
	bashPolicy tools.CommandPolicy

	mu       sync.Mutex
	planMode bool
	// promptFor rebuilds the system prompt per run workspace and model
	// (nil = SystemPrompt for every run); wsPrompts caches the results keyed
	// by workspace+model (bounded LRU) and is invalidated by SetSystemPrompt.
	promptFor func(workDir, model string) string
	wsPrompts *wsCache[string]
	// skillsForFn/agentsForFn/hooksForFn resolve the per-workspace skill
	// set, subagent definitions and hook runner (nil = the startup values);
	// the ws* caches bound their per-workspace results (LRU).
	skillsForFn func(workdir string) []skills.Skill
	wsSkills    *wsCache[[]skills.Skill]
	agentsForFn func(workdir string) []agents.Definition
	wsAgents    *wsCache[[]agents.Definition]
	hooksForFn  func(workdir string) *hooks.Runner
	wsHooks     *wsCache[*hooks.Runner]
	// spec tracks speculatively executed tool calls started while the model
	// was still streaming (streaming tool execution).
	spec speculativeRegistry
	// escalate rebuilds the model at the escalated output cap (optional).
	escalate func() (language.ChatModel, error)
	// toolBudget is the per-round tool-result character budget (0 disables).
	toolBudget int
	// inputs buffers mid-turn user input (Claude Code's queued commands).
	inputs queue
	// emitSummaries enables async tool-use summary generation; summaries
	// buffers them for non-blocking consumption.
	emitSummaries bool
	toolSummaries toolSummaryBuffer
	// subagent marks Task-spawned agents (see Config.Subagent).
	subagent bool
	// refreshTools re-collects extra tools between turns (optional).
	refreshTools func(ctx context.Context) []coretools.Tool
	refreshMu    sync.Mutex // serializes extra-tool swap commits only (no I/O under it)
	refreshSF    singleflight.Group
	refreshLast  atomic.Int64 // unix nano of last completed refresh
	// baseTools/extraTools split the tool list so refreshes only rebuild the
	// extra part (registry + built-ins stay fixed).
	baseTools  []coretools.Tool
	extraTools []coretools.Tool
	// memory is the optional long-term KV memoryStore for prefetch
	// attachments; wired into mem below.
	// maxTurns caps rounds per turn (0 = unlimited); turns counts rounds.
	maxTurns int
	turns    turnCounter
	// taskState tracks the periodic task summary (claude ps).
	taskState taskSummaryState
	// cron schedules session-scoped scheduled prompts (ScheduleCron); the
	// host dependency is injected as a notification sink at construction.
	cron *cronScheduler
	// goal holds the persistent-objective state and the running goal loop
	// (goal mode; see goal.go and goalgraph.go).
	goal goalManager
	// mcpAuth runs MCP OAuth flows (the McpAuth tool; optional).
	mcpAuth func(ctx context.Context, server string) error
	// obs records graph/state/session/message events (nil = disabled).
	obs *obslog.Store
	// usage accumulates provider-reported token counts across model calls.
	usage usageTracker
	// mem is the long-term-memory subsystem (engine, KV fallback, prefetch,
	// workspace override, System 2 throttle); see memorystore.go.
	mem memoryRuntime
	// loadSettings re-reads settings for per-run knobs (nil = defaults);
	// shared with the memory runtime and subagent construction.
	loadSettings func() settings.Settings
	// transcriptMu guards lastTranscript: the most recent model-round input
	// state, used by Task fork=true to copy the live conversation.
	transcriptMu   sync.Mutex
	lastTranscript []messages.Message
	// compactFails counts consecutive summarization failures; after
	// maxCompactFails auto-compaction is disabled until a manual compact
	// succeeds (mirrors Claude Code's auto-compact circuit breaker).
	compactFails int
}

// setTranscript records the newest model-round input state (fork source).
func (a *Agent) setTranscript(msgs []messages.Message) {
	cp := append([]messages.Message(nil), msgs...)
	a.transcriptMu.Lock()
	a.lastTranscript = cp
	a.transcriptMu.Unlock()
}

// transcriptSnapshot returns a copy of the latest conversation state.
func (a *Agent) transcriptSnapshot() []messages.Message {
	a.transcriptMu.Lock()
	defer a.transcriptMu.Unlock()
	return append([]messages.Message(nil), a.lastTranscript...)
}

// maxCompactFails is the number of consecutive failed summaries after which
// auto-compaction trips its breaker.
const maxCompactFails = 3

// New builds and compiles the coding-agent graph.
func New(cfg Config) (*Agent, error) {
	if cfg.Model == nil {
		return nil, fmt.Errorf("agent: model is required")
	}
	if cfg.Registry == nil {
		return nil, fmt.Errorf("agent: tool registry is required")
	}
	if cfg.SystemPrompt == "" {
		cfg.SystemPrompt = "You are a helpful coding assistant."
	}
	if cfg.PermissionMode == "" {
		cfg.PermissionMode = config.ModeDefault
	}
	if cfg.RecursionLimit <= 0 {
		cfg.RecursionLimit = 50
	}
	if cfg.WorkDir == "" {
		cfg.WorkDir = "."
	}

	a := &Agent{
		model:          cfg.Model,
		registry:       cfg.Registry,
		systemPrompt:   cfg.SystemPrompt,
		mode:           cfg.PermissionMode,
		workdir:        cfg.WorkDir,
		recursionLimit: cfg.RecursionLimit,
		subagentModel:  cfg.SubagentModel,
		tasks:          newTaskManager(),
		taskList:       newTaskList(),
		hooks:          cfg.Hooks,
		skills:         cfg.Skills,
		compactLimit:   cfg.CompactThreshold,
		summarizer:     cfg.Summarizer,
		bus:            cfg.Bus,
		agentName:      LeadName,
		perms:          cfg.Permissions,
		agentDefs:      cfg.Agents,
		bashPolicy:     cfg.BashPolicy,
		mcpAuth:        cfg.McpAuth,
		escalate:       cfg.MaxTokensEscalator,
		emitSummaries:  cfg.EmitToolUseSummaries,
		subagent:       cfg.Subagent,
		refreshTools:   cfg.RefreshTools,
		maxTurns:       cfg.MaxTurns,
		obs:            cfg.Obs,
		loadSettings:   cfg.LoadSettings,
		promptFor:      cfg.SystemPromptFor,
		skillsForFn:    cfg.SkillsFor,
		agentsForFn:    cfg.AgentsFor,
		hooksForFn:     cfg.HooksFor,
	}
	// The four per-workspace caches are bounded LRU (wsCacheCapacity) — the
	// sync.Map versions grew without bound across distinct workspaces.
	a.wsPrompts = newWSCache[string](wsCacheCapacity)
	a.wsSkills = newWSCache[[]skills.Skill](wsCacheCapacity)
	a.wsAgents = newWSCache[[]agents.Definition](wsCacheCapacity)
	a.wsHooks = newWSCache[*hooks.Runner](wsCacheCapacity)
	// The cron scheduler's only host dependency is the notification queue;
	// the method value is safe to capture before the rest of the wiring
	// because it is only invoked by the scheduler loop, never here.
	a.cron = newCronScheduler(a.QueueNotification)
	// The memory runtime takes its dependencies as inputs: the KV store,
	// the engine, the settings loader and a nil-safe obs log sink.
	a.mem.init(cfg.Memory, cfg.MemoryEngine, cfg.LoadSettings, a.obsLogSink())
	if cfg.ToolResultBudgetChars == 0 {
		a.toolBudget = DefaultToolResultBudgetChars
	} else {
		a.toolBudget = cfg.ToolResultBudgetChars
	}
	if a.subagentModel == nil {
		a.subagentModel = cfg.Model
	}

	// Wrap in a retry/fallback router when configured.
	runtimeModel := cfg.Model
	if cfg.FallbackModel != nil || cfg.ModelMaxRetries > 0 {
		router, err := model.New(model.Config{
			Primary:    cfg.Model,
			Fallback:   cfg.FallbackModel,
			MaxRetries: cfg.ModelMaxRetries,
		})
		if err != nil {
			return nil, err
		}
		runtimeModel = router
	}

	base := cfg.Registry.All()
	if cfg.Subagent {
		a.toolList = filterTools(base, "AskUserQuestion", "EnterPlanMode", "ExitPlanMode", "Task", "TaskOutput", "TaskStop", "Skill", "TaskCreate", "TaskGet", "TaskUpdate", "TaskList", "ScheduleCron", "GetGoal", "UpdateGoal")
		a.toolList = append(a.toolList, a.memoryTools()...) // self-crops to read-only
	} else {
		a.toolList = append(append([]coretools.Tool(nil), base...), a.taskTools()...)
		a.toolList = append(a.toolList, a.skillTool()...)
		a.toolList = append(a.toolList, a.taskListTools()...)
		a.toolList = append(a.toolList, a.sendMessageTool()...)
		a.toolList = append(a.toolList, a.teamTools()...)
		a.toolList = append(a.toolList, a.scheduleCronTool()...)
		a.toolList = append(a.toolList, a.worktreeTools()...)
		a.toolList = append(a.toolList, a.mcpAuthTool()...)
		a.toolList = append(a.toolList, a.goalTools()...)
		a.toolList = append(a.toolList, a.memoryTools()...)
		a.toolList = append(a.toolList, cfg.ExtraTools...)
	}
	// Split base/extra so between-turn refreshes only rebuild the extra
	// (e.g. MCP) part of the tool set.
	a.extraTools = append([]coretools.Tool(nil), cfg.ExtraTools...)
	a.baseTools = append([]coretools.Tool(nil), a.toolList[:len(a.toolList)-len(a.extraTools)]...)
	a.toolByName = make(map[string]coretools.Tool, len(a.toolList))
	for _, t := range a.toolList {
		a.toolByName[t.Name()] = t
	}

	bound := runtimeModel
	if len(a.toolList) > 0 && bound.Capabilities().ToolCalling {
		b, err := bound.BindTools(a.toolList)
		if err != nil {
			return nil, fmt.Errorf("agent: bind tools: %w", err)
		}
		bound = b
	}
	a.boundModel = bound

	g := graph.NewStateGraph()
	g.AddReducer("messages", MessagesReducer)
	g.AddStreamingNode(nodeModel, a.modelNode())
	if len(a.toolList) > 0 {
		g.AddNode(nodeTools, a.toolsNode())
		g.AddEdge(nodeTools, nodeModel)
	}
	g.AddConditionalEdges(nodeModel, a.route)
	g.SetEntryPoint(nodeModel)

	compileOpts := []graph.CompileOption{graph.WithRecursionLimit(cfg.RecursionLimit)}
	if a.obs != nil {
		compileOpts = append(compileOpts, graph.WithObserver("agent", a.obs))
	}
	switch {
	case cfg.Checkpointer != nil:
		compileOpts = append(compileOpts, graph.WithCheckpointer(cfg.Checkpointer))
	case cfg.CheckpointDir != "":
		compileOpts = append(compileOpts, graph.WithCheckpointer(graph.NewFileCheckpointer(cfg.CheckpointDir)))
	}
	cg, err := g.Compile(compileOpts...)
	if err != nil {
		return nil, err
	}
	a.graph = cg
	// Background-shell completions (Bash run_in_background) are delivered as
	// tagged queue notifications after the next tool round.
	a.registry.Background().SetNotify(func(text string) {
		a.QueueNotification("background-task-completed", strings.TrimSpace(text))
	})
	return a, nil
}

// Graph returns the compiled graph for streaming/resume.
func (a *Agent) Graph() *graph.CompiledGraph { return a.graph }

// Obs returns the observability store (nil when not wired).
func (a *Agent) Obs() *obslog.Store { return a.obs }

// SetObsRun scopes subsequent observability events to one workspace/session/
// run. Harnesses call it at run start; recording itself is gated by the
// store's enabled flag.
func (a *Agent) SetObsRun(workspace, session, run string) {
	if a.obs == nil {
		return
	}
	name := workspace
	if name != "" {
		name = filepath.Base(name)
	}
	a.obs.SetContext(workspace, name, session, run)
}

// SetMemoryWorkspace scopes memory-engine calls to one workspace for runs
// without a ctx-scoped value: the TUI/REPL pins it to the workdir at startup
// and subagents inherit the parent's value at fork. The web server does NOT
// set it per-run (concurrent runs would race on this agent-wide field);
// instead each serve-mode run wraps its ctx with WithMemoryWorkspace, which
// resolution prefers (ctx value → this field → workdir; see memws.go).
func (a *Agent) SetMemoryWorkspace(ws string) {
	a.mem.SetWorkspace(ws)
}

// memoryWorkspace resolves the run workspace fallback chain tail: the
// runtime's explicit override, else the agent workdir. The ctx-value head
// of the chain is applied by memoryWorkspaceCtx.
func (a *Agent) memoryWorkspace() string {
	if ws, ok := a.mem.Workspace(); ok {
		return ws
	}
	return a.workdir
}

// MemoryWorkspace returns the current memory workspace (exported for the
// console and web wiring).
func (a *Agent) MemoryWorkspace() string { return a.memoryWorkspace() }

// MemoryEngine returns the current memory engine (nil = disabled).
func (a *Agent) MemoryEngine() memory.Engine {
	return a.mem.Engine()
}

// SetMemoryEngine swaps the memory engine whole-pointer (settings rebuild).
// In-flight calls keep the old engine; the caller delays Close.
func (a *Agent) SetMemoryEngine(m memory.Engine) {
	a.mem.SetEngine(m)
}

// obsLogSink adapts the observability store into the nil-safe log function
// the memory runtime expects.
func (a *Agent) obsLogSink() func(level, event string, fields map[string]any) {
	return func(level, event string, fields map[string]any) {
		if a.obs != nil {
			a.obs.Log(level, event, fields)
		}
	}
}

// beginMemoryPrefetch starts the turn's relevant-memory recall; the host
// resolves the workspace (ctx value → override → workdir) and hands it over.
func (a *Agent) beginMemoryPrefetch(ctx context.Context, msgs []messages.Message) {
	a.mem.Begin(ctx, msgs, a.memoryWorkspaceCtx(ctx))
}

// takeMemoryAttachments consumes the settled prefetch as attachment
// messages (nil-safe when nothing settled). The run-scoped ctx wins; a
// scope-less ctx (CLI single-run paths) falls back to the agent-level slot.
func (a *Agent) takeMemoryAttachments(ctx context.Context) []messages.Message {
	return a.mem.TakeAttachments(ctx)
}

// SystemPrompt returns the agent's system prompt.
func (a *Agent) SystemPrompt() string { return a.systemPrompt }

// Mode returns the current permission mode.
func (a *Agent) Mode() config.PermissionMode {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.mode
}

// SetMode sets the permission mode. Safe to call while a run is in flight
// (the web API does); checkPermission reads the mode under the same mutex.
func (a *Agent) SetMode(m config.PermissionMode) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.mode = m
}

// PlanMode reports whether the agent is currently in plan mode.
func (a *Agent) PlanMode() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.planMode
}

func (a *Agent) setPlanMode(v bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.planMode = v
}

// SetPlanMode sets the plan-mode state (used to restore a resumed session).
func (a *Agent) SetPlanMode(v bool) { a.setPlanMode(v) }

// SetTodos restores the todo list from a persisted snapshot.
func (a *Agent) SetTodos(todos []tools.Todo) { a.registry.TodoStore().Set(todos) }

// SetTasks restores the task list from a persisted snapshot.
func (a *Agent) SetTasks(tasks []TaskRecord) { a.currentTaskList().Restore(tasks) }

// Todos returns the current todo list.
func (a *Agent) Todos() []tools.Todo { return a.registry.TodoStore().Get() }

// Tasks returns the current task list.
func (a *Agent) Tasks() []TaskRecord { return a.currentTaskList().List() }

// Skills returns the loaded skills.
func (a *Agent) Skills() []skills.Skill { return a.skills }

// SetSkills replaces the loaded skills at runtime (tests, hot reload).
func (a *Agent) SetSkills(list []skills.Skill) { a.skills = list }

// HookSummary lists configured hook events (for /doctor); nil when no hook
// runner is attached.
func (a *Agent) HookSummary() []string {
	if a.hooks == nil {
		return nil
	}
	return a.hooks.Configured()
}

// PermissionRuleCounts reports the explicit allow/deny/ask rule counts (for
// /doctor); zeros when no rules file was loaded.
func (a *Agent) PermissionRuleCounts() (allow, deny, ask int) {
	if a.perms == nil {
		return 0, 0, 0
	}
	return len(a.perms.Allow), len(a.perms.Deny), len(a.perms.Ask)
}

// AgentDefinitions returns the loaded custom subagent definitions (/agents).
func (a *Agent) AgentDefinitions() []agents.Definition { return a.agentDefs }

// SkillsIn resolves the skill set visible to a run with the given context
// (workspace-scoped; exported for the web completions endpoint).
func (a *Agent) SkillsIn(ctx context.Context) []skills.Skill { return a.skillsFor(ctx) }

// AgentDefinitionsIn resolves the custom subagent definitions visible to a
// run with the given context (workspace-scoped).
func (a *Agent) AgentDefinitionsIn(ctx context.Context) []agents.Definition { return a.agentsFor(ctx) }

// SetAgentDefinitions replaces the custom subagent definitions at runtime
// (used by the /agents manager to refresh after edits; call when idle).
func (a *Agent) SetAgentDefinitions(defs []agents.Definition) { a.agentDefs = defs }

// SetSystemPrompt swaps the system prompt at runtime (the /output-style
// command). When a SystemPromptFor builder is wired the builder owns the
// prompt: the swap degrades to a cache invalidation so the next run rebuilds
// with live settings. Call when idle, not while a run is in flight.
func (a *Agent) SetSystemPrompt(s string) {
	if s == "" {
		return
	}
	a.mu.Lock()
	a.systemPrompt = s
	a.mu.Unlock()
	a.InvalidateWorkspacePrompts()
}

// InvalidateWorkspacePrompts drops the cached per-workspace system prompts so
// the next run in each workspace rebuilds them (style or model changes).
func (a *Agent) InvalidateWorkspacePrompts() {
	a.wsPrompts.Clear()
}

// systemPromptFor resolves the system prompt for one run. With a builder
// wired, every run (base workspace included) gets a prompt built for its
// workspace and the active model, cached per workspace+model — swapping the
// model (SetModel) therefore refreshes the model name without any explicit
// invalidation. Without a builder the startup prompt applies as-is.
func (a *Agent) systemPromptFor(ctx context.Context) string {
	if a.promptFor == nil {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.systemPrompt
	}
	ws := a.workdir
	if v, ok := runws.From(ctx); ok && v != "" {
		ws = v
	}
	model := a.CurrentModelName()
	key := ws + "\x00" + model
	if p, ok := a.wsPrompts.Get(key); ok {
		return p
	}
	p := a.promptFor(ws, model)
	a.wsPrompts.Set(key, p)
	return p
}

// skillsFor resolves the skill set for the calling run's workspace (the
// startup set when no loader is wired or the run carries no workspace).
func (a *Agent) skillsFor(ctx context.Context) []skills.Skill {
	if a.skillsForFn == nil {
		return a.skills
	}
	ws, ok := runws.From(ctx)
	if !ok || ws == "" {
		return a.skills
	}
	if v, ok := a.wsSkills.Get(ws); ok {
		return v
	}
	list := a.skillsForFn(ws)
	a.wsSkills.Set(ws, list)
	return list
}

// agentsFor resolves custom subagent definitions for the calling run's
// workspace (the startup set when no loader is wired).
func (a *Agent) agentsFor(ctx context.Context) []agents.Definition {
	if a.agentsForFn == nil {
		return a.agentDefs
	}
	ws, ok := runws.From(ctx)
	if !ok || ws == "" {
		return a.agentDefs
	}
	if v, ok := a.wsAgents.Get(ws); ok {
		return v
	}
	list := a.agentsForFn(ws)
	a.wsAgents.Set(ws, list)
	return list
}

// hooksFor resolves the hook runner for the calling run's workspace (the
// startup runner when no loader is wired). May return nil; every call site
// nil-checks before use.
func (a *Agent) hooksFor(ctx context.Context) *hooks.Runner {
	if a.hooksForFn == nil {
		return a.hooks
	}
	ws, ok := runws.From(ctx)
	if !ok || ws == "" {
		return a.hooks
	}
	if v, ok := a.wsHooks.Get(ws); ok {
		return v
	}
	h := a.hooksForFn(ws)
	a.wsHooks.Set(ws, h)
	return h
}

// NewState builds the initial graph state: the run's system prompt followed
// by the provided conversation history. The context carries the run
// workspace (see WithRunWorkspace), which selects the environment the model
// is told about.
func (a *Agent) NewState(ctx context.Context, history []messages.Message) graph.State {
	msgs := make([]messages.Message, 0, len(history)+1)
	msgs = append(msgs, messages.System(a.systemPromptFor(ctx)))
	msgs = append(msgs, history...)
	return graph.State{"messages": msgs}
}

// CompactThreshold returns the configured auto-compaction token threshold
// (0 = disabled).
func (a *Agent) CompactThreshold() int { return a.compactLimit }

// ToolNames returns the sorted names of every tool the agent can call
// (built-ins plus MCP extras).
func (a *Agent) ToolNames() []string {
	a.mu.Lock()
	list := a.toolList
	a.mu.Unlock()
	names := make([]string, 0, len(list))
	for _, t := range list {
		names = append(names, t.Name())
	}
	sort.Strings(names)
	return names
}

// SetModel swaps the chat model used by the model node at runtime, rebinding
// the tool set. Safe to call while a run is in flight (the settings API and
// thinking-level endpoint do): the model snapshot is taken under a.mu and the
// swap is committed under it.
func (a *Agent) SetModel(m language.ChatModel) error {
	if m == nil {
		return fmt.Errorf("agent: model is required")
	}
	a.mu.Lock()
	tools := a.toolList
	a.mu.Unlock()
	bound := m
	if len(tools) > 0 && bound.Capabilities().ToolCalling {
		b, err := bound.BindTools(tools)
		if err != nil {
			return fmt.Errorf("agent: bind tools: %w", err)
		}
		bound = b
	}
	a.mu.Lock()
	a.model = m
	a.boundModel = bound
	a.mu.Unlock()
	return nil
}

// Invoke runs the agent to completion and returns the full message transcript
// (system prompt included). A prompt-too-long failure first triggers reactive
// compaction with a single retry (RecoverPromptTooLong semantics); a truncated
// final response is then recovered per RecoverOutputLimit.
func (a *Agent) Invoke(ctx context.Context, history []messages.Message) ([]messages.Message, error) {
	a.ResetTurnCounter() // one Invoke = one turn for MaxTurns accounting
	out, err := a.runTurn(ctx, history)
	if err != nil {
		if !IsPromptTooLongError(err) {
			return nil, err
		}
		compacted := a.ReactiveCompactHistory(ctx, history)
		if !CompactionShrank(history, compacted) {
			return nil, err // nothing to compact; surface the provider error
		}
		// Single retry on the compacted history — the
		// hasAttemptedReactiveCompact guard: a second prompt-too-long error
		// here is returned as-is.
		history = compacted
		out, err = a.runTurn(ctx, history)
		if err != nil {
			return nil, err
		}
	}
	return a.RecoverOutputLimit(ctx, history, out, a.runTurn)
}

// runTurn compacts history and runs one full graph turn.
func (a *Agent) runTurn(ctx context.Context, history []messages.Message) ([]messages.Message, error) {
	history = a.CompactHistory(ctx, history)
	state, err := a.graph.Invoke(ctx, a.NewState(ctx, history))
	if err != nil {
		return nil, err
	}
	return MessagesFrom(state), nil
}

// CurrentModel returns the active chat model (before tool binding).
func (a *Agent) CurrentModel() language.ChatModel {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.model
}

// CurrentModelName returns a display name for the active model (observability
// events; falls back to the struct's type when unnamed).
func (a *Agent) CurrentModelName() string {
	if m, ok := a.CurrentModel().(interface{ Name() string }); ok {
		return m.Name()
	}
	return fmt.Sprintf("%T", a.CurrentModel())
}

// Stream runs the agent and streams graph progress steps.
func (a *Agent) Stream(ctx context.Context, history []messages.Message) (graph.Stream[graph.Step], error) {
	history = a.CompactHistory(ctx, history)
	return a.graph.Stream(ctx, a.NewState(ctx, history))
}

// ResumeCheckpoint resumes a run from a persisted checkpoint key (requires
// CheckpointDir to have been configured).
func (a *Agent) ResumeCheckpoint(ctx context.Context, key string, resumeValue any) ([]messages.Message, error) {
	state, err := a.graph.ResumeCheckpoint(ctx, key, resumeValue)
	if err != nil {
		return nil, err
	}
	return MessagesFrom(state), nil
}

// CompactHistory compacts history when auto-compaction is enabled and the
// token estimate exceeds the threshold; otherwise it returns history unchanged.
// It first snips old tool results and micro-compacts large ones, then (if still
// over budget) summarizes the head. After maxCompactFails consecutive
// summarization failures the breaker trips and auto-compaction is skipped.
func (a *Agent) CompactHistory(ctx context.Context, history []messages.Message) []messages.Message {
	if a.compactLimit <= 0 || a.summarizer == nil {
		return history
	}
	if a.compactFails >= maxCompactFails {
		return history // breaker tripped; /compact can still force it
	}
	return a.compact(ctx, history, false)
}

// ForceCompact compacts history on demand (the console /compact command),
// ignoring — and on success resetting — the auto-compaction breaker.
func (a *Agent) ForceCompact(ctx context.Context, history []messages.Message) []messages.Message {
	if a.summarizer == nil {
		return history
	}
	return a.compact(ctx, history, true)
}

// compact runs the compaction pipeline. force bypasses a tripped breaker.
func (a *Agent) compact(ctx context.Context, history []messages.Message, force bool) []messages.Message {
	if h := a.hooksFor(ctx); h != nil {
		if _, blocked, err := h.PreCompact(ctx); err == nil && blocked {
			return history // PreCompact hook blocked compaction
		}
	}
	history = compact.Snip(history, a.compactLimit*4)
	history = compact.Microcompact(history, 10, 2000)
	if compact.EstimateTokens(history) <= a.compactLimit {
		return history
	}
	out, err := compact.Compact(ctx, history, a.compactLimit, SummarizeWithModel(a.summarizer))
	if err != nil {
		if !force {
			a.compactFails++
		}
		return history
	}
	a.compactFails = 0
	if h := a.hooksFor(ctx); h != nil {
		_, _ = h.PostCompact(ctx)
	}
	return out
}

// SummarizeWithModel builds a compact.SummaryWriter from a chat model.
func SummarizeWithModel(model language.ChatModel) compact.SummaryWriter {
	return func(ctx context.Context, msgs []messages.Message) (string, error) {
		prompt := "Summarize this conversation concisely, preserving key facts, decisions, and any pending work:\n\n" + messages.BufferString(msgs)
		resp, err := model.Invoke(ctx, []messages.Message{messages.Human(prompt)})
		if err != nil {
			return "", err
		}
		return resp.Content, nil
	}
}

// RunStopHooks runs Stop hooks, returning feedback and whether continuation
// was blocked (a hook exited 2).
func (a *Agent) RunStopHooks(ctx context.Context, reason string) (string, bool, error) {
	h := a.hooksFor(ctx)
	if h == nil {
		return "", false, nil
	}
	return h.Stop(ctx, reason)
}

// RunUserPromptSubmit runs UserPromptSubmit hooks, returning injected context.
func (a *Agent) RunUserPromptSubmit(ctx context.Context, prompt string) (string, error) {
	h := a.hooksFor(ctx)
	if h == nil {
		return "", nil
	}
	return h.UserPromptSubmit(ctx, prompt)
}

// RunSessionStart runs SessionStart hooks, returning feedback.
func (a *Agent) RunSessionStart(ctx context.Context) (string, error) {
	h := a.hooksFor(ctx)
	if h == nil {
		return "", nil
	}
	return h.SessionStart(ctx)
}

// RunSessionEnd runs SessionEnd hooks (session teardown: exit, EOF, or
// one-shot completion). Best-effort; the returned error is usually only
// logged by the caller.
func (a *Agent) RunSessionEnd(ctx context.Context, reason string) error {
	h := a.hooksFor(ctx)
	if h == nil {
		return nil
	}
	_, err := h.SessionEnd(ctx, reason)
	return err
}

// modelNode streams the (tool-bound) model, emitting text deltas and
// accumulating the full message (including tool calls). Tool calls that
// arrive stream-complete are started in the background (streaming tool
// execution) so they overlap with the remaining model output. When the
// model router restarts the stream (transient-error retry or fallback
// model), the restart marker resets the accumulated partial response —
// Claude Code's tombstone equivalent — and cancels the speculative calls
// dispatched from the failed attempt.
func (a *Agent) modelNode() graph.StreamingNodeFunc {
	return func(ctx context.Context, state graph.State, emit func(any)) (any, error) {
		msgs := MessagesFrom(state)
		// Record the conversation for Task fork=true (Claude Code's
		// forkSubagent copies the live transcript into the subagent).
		a.setTranscript(msgs)
		// Relevant-memory prefetch (Claude Code's turn-start prefetch): the
		// store is searched in the background while the model streams; the
		// attachments are consumed after tool rounds.
		a.beginMemoryPrefetch(ctx, msgs)
		// Tool-result budget runs before every model request (Claude Code's
		// stage 8): oversized rounds are preview-replaced so the prompt fits.
		// The transcript/state keeps the originals.
		msgs = ApplyToolResultBudget(msgs, a.toolBudget)
		// Snapshot the bound model under a.mu: SetModel and tool refreshes
		// swap it concurrently (settings API, plugin toggles).
		a.mu.Lock()
		bound := a.boundModel
		a.mu.Unlock()
		stream, err := bound.Stream(ctx, msgs)
		if err != nil {
			return nil, err
		}
		defer stream.Close()

		var full messages.Message
		var specIDs []string
		for {
			chunk, ok, err := stream.Next(ctx)
			if err != nil {
				return nil, err
			}
			if !ok {
				break
			}
			if model.IsRestartChunk(chunk) {
				// The failed attempt's partial response is discarded:
				// reset the accumulation and abort its speculative calls.
				if len(specIDs) > 0 {
					a.spec.cancel(specIDs...)
					specIDs = nil
				}
				full = messages.Message{}
				if emit != nil {
					emit("\n[" + model.RestartReason(chunk) + "]\n")
				}
				continue
			}
			if text := TextDelta(chunk); text != "" && emit != nil {
				emit(text)
			}
			if r := ReasoningDeltaText(chunk); r != "" && emit != nil {
				emit(ReasoningDelta{Text: r})
			}
			MergeStreamChunks(&full, chunk)
			// Providers emit each tool call as one finished chunk when its
			// content block closes; dispatch it while the stream continues.
			for _, tc := range chunk.ToolCalls {
				if a.dispatchSpeculative(ctx, tc) {
					specIDs = append(specIDs, tc.ID)
				}
			}
		}
		if full.Role == "" {
			full = messages.AI("")
		}
		// Session usage accounting (Claude Code's cost tracker): fold this
		// call's reported tokens into the running totals for /cost.
		a.usage.recordUsage(full.UsageMetadata)
		if a.obs != nil {
			a.obs.LogUsage(a.CurrentModelName(), full.UsageMetadata)
		}
		// PostSampling hooks analyze the response asynchronously (Claude
		// Code's post-sampling evaluators); detached from the stream ctx so
		// graph teardown does not kill an in-flight analysis.
		if h := a.hooksFor(ctx); h != nil && (full.Content != "" || len(full.ToolCalls) > 0) {
			resp := full.Content
			go func() { _ = h.PostSampling(context.WithoutCancel(ctx), resp) }()
		}
		return graph.State{"messages": []messages.Message{full}}, nil
	}
}

// toolsNode executes pending tool calls, applying the interaction gate and
// Pre/Post tool-use hooks.
func (a *Agent) toolsNode() graph.NodeFunc {
	return func(ctx context.Context, state graph.State) (any, error) {
		msgs := MessagesFrom(state)
		if len(msgs) == 0 {
			return nil, nil
		}
		last := msgs[len(msgs)-1]
		if last.Role != messages.RoleAI || len(last.ToolCalls) == 0 {
			return nil, nil
		}

		calls := last.ToolCalls
		run := make([]bool, len(calls))
		overrides := make([]*coretools.Result, len(calls))
		preFeedback := make([]string, len(calls))
		futures := make([]*toolFuture, len(calls))

		// Gates and PreToolUse hooks run sequentially so interrupts pause
		// deterministically. Calls already speculatively executed during
		// model streaming (permission decided then) skip the gate: the tools
		// node only awaits their result.
		for i, call := range calls {
			if f := a.spec.get(call.ID); f != nil {
				futures[i] = f
				continue
			}
			shouldRun, override, err := a.gate(ctx, call)
			if err != nil {
				return nil, err
			}
			if override != nil {
				overrides[i] = override
				continue
			}
			if !shouldRun {
				run[i] = false
				continue
			}
			if h := a.hooksFor(ctx); h != nil {
				d, err := h.PreToolUse(ctx, call.Name, call.Args)
				if err != nil {
					return nil, err
				}
				if !d.Allowed {
					content := "Error: tool use denied by hook"
					if d.Feedback != "" {
						content = "Error: " + d.Feedback
					}
					overrides[i] = &coretools.Result{Content: content}
					continue
				}
				preFeedback[i] = d.Feedback
			}
			run[i] = true
		}

		results := make([]messages.Message, len(calls))
		var wg sync.WaitGroup
		for i, call := range calls {
			if futures[i] != nil {
				results[i] = futures[i].wait()
				continue
			}
			if overrides[i] != nil {
				results[i] = messages.Tool(call.ID, overrides[i].Content)
				continue
			}
			if !run[i] {
				results[i] = messages.Tool(call.ID, "Error: permission denied")
				continue
			}
			wg.Add(1)
			go func(i int, call messages.ToolCall) {
				defer wg.Done()
				res := a.execute(ctx, call.Name, call.Args)
				content := res.Content
				if preFeedback[i] != "" {
					content = preFeedback[i] + "\n" + content
				}
				if h := a.hooksFor(ctx); h != nil {
					if p, err := h.PostToolUse(ctx, call.Name, call.Args, content); err == nil && p.Feedback != "" {
						content += "\n" + p.Feedback
					}
				}
				results[i] = toolMessageFromResult(call.ID, content, &res)
			}(i, call)
		}
		wg.Wait()
		// Results are merged into state: retire the consumed futures. On the
		// interrupt path above they remain for a same-process resume.
		for i := range calls {
			if futures[i] != nil {
				a.spec.consume(calls[i].ID)
			}
		}
		// Mid-turn user input (Claude Code's stage 27): anything queued while
		// the tools ran is appended strictly after the result batch — the API
		// forbids interleaving tool results with user messages — so the model
		// addresses it in this turn. Drained after execution, so input
		// submitted during the tools' run is included; an interrupt-resume
		// re-execution drains an already-emptied queue and adds nothing.
		for _, q := range a.inputs.drain() {
			results = append(results, formatQueuedInput(q))
		}
		// Kick off the async tool-use summary (stage 24): UI-only, generated
		// in the background with the summarizer while the next model round
		// runs; consumed via TakeReadyToolUseSummaries.
		a.beginToolUseSummary(ctx, calls, results, last.Content)
		// Periodic task summary (claude ps): refresh what the agent is
		// working on every few rounds.
		a.maybeBeginTaskSummary(ctx, msgs)
		// Relevant-memory attachments (stage 27): consume the turn's
		// prefetch after the result batch.
		results = append(results, a.takeMemoryAttachments(ctx)...)
		// Between-turn tool refresh (stage 28): re-collect extra tools so
		// hot-added ones are available in the next round. Non-blocking: a
		// slow MCP re-list runs in the background (singleflight-deduped,
		// TTL-throttled) and the swap commits when it lands.
		a.maybeRefreshTools(ctx)
		// MaxTurns (stage 28): this tool round completes a model round; at
		// the cap, append the marker and short-circuit to END — the tools
		// node otherwise has an unconditional edge back to the model.
		a.turns.mu.Lock()
		a.turns.count++
		atCap := a.maxTurns > 0 && a.turns.count >= a.maxTurns
		rounds := a.turns.count
		a.turns.mu.Unlock()
		if atCap {
			results = append(results, messages.Human(maxTurnsMarker(rounds)))
			return graph.Command{Update: graph.State{"messages": results}, Goto: graph.END}, nil
		}
		return graph.State{"messages": results}, nil
	}
}

// gate decides whether a tool call should run, or produces an override result
// for harness-handled interactions (AskUserQuestion, plan mode). It may
// interrupt the graph.
func (a *Agent) gate(ctx context.Context, call messages.ToolCall) (run bool, override *coretools.Result, err error) {
	switch call.Name {
	case "AskUserQuestion":
		var q struct {
			Questions []tools.Question `json:"questions"`
		}
		if err := decodeArgs(call.Args, &q); err != nil {
			return false, nil, err
		}
		answerAny, err := graph.Interrupt(ctx, AskUserRequest{Questions: q.Questions})
		if err != nil {
			return false, nil, err
		}
		answer, _ := answerAny.(AskUserAnswer)
		res := formatAskUserAnswers(q.Questions, answer.Answers)
		return false, &coretools.Result{Content: res}, nil

	case "EnterPlanMode":
		a.setPlanMode(true)
		res := coretools.Result{Content: "Plan mode enabled. Use read-only tools to research and write a plan, then call ExitPlanMode to request approval before implementing."}
		return false, &res, nil

	case "ExitPlanMode":
		if !a.PlanMode() {
			res := coretools.Result{Content: "Not currently in plan mode."}
			return false, &res, nil
		}
		var p struct {
			Plan string `json:"plan"`
		}
		if err := decodeArgs(call.Args, &p); err != nil {
			return false, nil, err
		}
		answerAny, err := graph.Interrupt(ctx, PlanApprovalRequest{Plan: p.Plan})
		if err != nil {
			return false, nil, err
		}
		if parseAnswer(answerAny) {
			a.setPlanMode(false)
			res := coretools.Result{Content: "Plan approved by the user. Proceeding with implementation."}
			return false, &res, nil
		}
		res := coretools.Result{Content: "Plan rejected by the user. Revise your plan based on their feedback and call ExitPlanMode again."}
		return false, &res, nil
	}

	allowed, err := a.checkPermission(ctx, call)
	if err != nil {
		return false, nil, err
	}
	if !allowed {
		res := coretools.Result{Content: "Error: permission denied by user"}
		return false, &res, nil
	}
	return true, nil, nil
}

// execute runs a tool by name from the agent's full tool set.
func (a *Agent) execute(ctx context.Context, name string, args map[string]any) coretools.Result {
	if name == "Bash" {
		if cmd, ok := args["command"].(string); ok {
			if allowed, reason := a.bashPolicy.Allow(cmd); !allowed {
				return coretools.Result{Content: "Error: " + reason}
			}
		}
	}
	a.mu.Lock()
	t, ok := a.toolByName[name]
	a.mu.Unlock()
	if !ok {
		return coretools.Result{Content: fmt.Sprintf("Error: unknown tool %q", name)}
	}
	res, err := t.Invoke(ctx, args)
	if err != nil {
		return coretools.Result{Content: fmt.Sprintf("Error: %v", err)}
	}
	return res
}

// route decides whether to run tools or end the turn.
func (a *Agent) route(ctx context.Context, state graph.State) (string, error) {
	msgs := MessagesFrom(state)
	if len(msgs) == 0 {
		return graph.END, nil
	}
	last := msgs[len(msgs)-1]
	a.mu.Lock()
	hasTools := len(a.toolList) > 0
	a.mu.Unlock()
	if last.Role == messages.RoleAI && len(last.ToolCalls) > 0 && hasTools {
		return nodeTools, nil
	}
	return graph.END, nil
}

// checkPermission decides whether a tool call may run. Explicit permission
// rules (deny/allow/ask) are evaluated first, then the permission mode. It is
// input-sensitive: read-only commands auto-allow, writes/shell ask in default
// mode.
func (a *Agent) checkPermission(ctx context.Context, call messages.ToolCall) (bool, error) {
	if a.perms != nil {
		if action, ok := a.perms.Action(call.Name, call.Args); ok {
			switch action {
			case permissions.ActionDeny:
				return false, nil
			case permissions.ActionAllow:
				return true, nil
			case permissions.ActionAsk:
				return a.askPermission(ctx, call)
			}
		}
	}

	readOnly := tools.IsReadOnly(call.Name, call.Args)
	cat := tools.CategoryOf(call.Name)

	a.mu.Lock()
	mode := a.mode
	a.mu.Unlock()
	switch mode {
	case config.ModeBypass:
		return true, nil
	case config.ModePlan, config.ModeReadOnly:
		return readOnly || cat == tools.CatMeta, nil
	case config.ModeAcceptEdits:
		if cat == tools.CatWrite {
			return true, nil
		}
		if readOnly || cat == tools.CatMeta {
			return true, nil
		}
		return a.askPermission(ctx, call)
	default: // ModeDefault
		if readOnly || cat == tools.CatMeta {
			return true, nil
		}
		return a.askPermission(ctx, call)
	}
}

func (a *Agent) askPermission(ctx context.Context, call messages.ToolCall) (bool, error) {
	// PermissionRequest hooks run before the interrupt: a hook may
	// pre-approve or pre-deny without bothering the user.
	if h := a.hooksFor(ctx); h != nil {
		d, err := h.PermissionRequest(ctx, call.Name, call.Args)
		if err == nil && d.Decision != "" {
			return d.Decision == "allow", nil
		}
	}
	req := PermissionRequest{
		ToolName:   call.Name,
		ToolArgs:   call.Args,
		Reason:     toolReason(call),
		IsReadOnly: tools.IsReadOnly(call.Name, call.Args),
	}
	answer, err := graph.Interrupt(ctx, req)
	if err != nil {
		return false, err
	}
	return parseAnswer(answer), nil
}

// newSubagent builds a Task-spawned subagent reusing the same model and
// permission mode, with the subagent prompt and a reduced tool set. It
// returns an error instead of panicking: the callers run inside tool
// goroutines, where a panic would take down the whole process. The context
// carries the parent run's workspace, which the subagent inherits (its
// prompt, tools and memory calls stay scoped to the same session).
func (a *Agent) newSubagent(ctx context.Context, def *agents.Definition) (*Agent, error) {
	ws := a.memoryWorkspaceCtx(ctx)
	reg := tools.NewRegistry(ws)
	sysPrompt := prompt.SubagentPrompt(ws)
	if def != nil && def.Prompt != "" {
		sysPrompt = def.Prompt
	}
	a.mu.Lock()
	mode := a.mode
	a.mu.Unlock()
	sub, err := New(Config{
		Model:          a.subagentModel,
		Registry:       reg,
		SystemPrompt:   sysPrompt,
		PermissionMode: mode,
		RecursionLimit: a.recursionLimit,
		WorkDir:        ws,
		Subagent:       true,
		MemoryEngine:   a.MemoryEngine(), // read-only memory tools stay available
		LoadSettings:   a.loadSettings,
	})
	if err != nil {
		return nil, fmt.Errorf("agent: build subagent: %w", err)
	}
	if def != nil && len(def.Tools) > 0 {
		sub.toolList = allowTools(sub.toolList, def.Tools)
		sub.toolByName = make(map[string]coretools.Tool, len(sub.toolList))
		for _, t := range sub.toolList {
			sub.toolByName[t.Name()] = t
		}
		if len(sub.toolList) > 0 && sub.model.Capabilities().ToolCalling {
			if b, err := sub.model.BindTools(sub.toolList); err == nil {
				sub.boundModel = b
			}
		}
	}
	// Subagents share the parent's memory workspace so reads stay scoped to
	// the same run (writes are already cropped out of their tool set).
	sub.SetMemoryWorkspace(ws)
	return sub, nil
}

// allowTools keeps only the tools whose names are in the allowed set.
func allowTools(list []coretools.Tool, allowed []string) []coretools.Tool {
	keep := make(map[string]bool, len(allowed))
	for _, n := range allowed {
		keep[n] = true
	}
	out := make([]coretools.Tool, 0, len(list))
	for _, t := range list {
		if keep[t.Name()] {
			out = append(out, t)
		}
	}
	return out
}

func toolReason(call messages.ToolCall) string {
	switch call.Name {
	case "Bash":
		if c, ok := call.Args["command"].(string); ok {
			if tools.IsDestructiveCommand(c) {
				return "Run destructive command (⚠): " + c
			}
			return "Run command: " + c
		}
	case "Write", "Edit":
		if p, ok := call.Args["file_path"].(string); ok {
			return call.Name + " " + p
		}
	case "remember_fact":
		if c, ok := call.Args["content"].(string); ok {
			return call.Name + ": " + c
		}
	case "index_code":
		root := "(working directory)"
		if r, ok := call.Args["root"].(string); ok && r != "" {
			root = r
		}
		return "Index source into memory DB: " + root +
			" (reads source tree; writes only to the memory DB)"
	}
	return call.Name
}

func parseAnswer(v any) bool {
	switch s := v.(type) {
	case bool:
		return s
	case string:
		s = strings.ToLower(strings.TrimSpace(s))
		return s == "y" || s == "yes" || s == "allow" || s == "approve" || s == "a" || s == "true" || s == "1"
	default:
		return false
	}
}

func formatAskUserAnswers(questions []tools.Question, answers []string) string {
	var b strings.Builder
	for i, q := range questions {
		ans := "(no answer)"
		if i < len(answers) {
			ans = answers[i]
		}
		fmt.Fprintf(&b, "Q: %s\nA: %s\n", q.Question, ans)
	}
	return b.String()
}

func decodeArgs(args map[string]any, out any) error {
	raw, err := json.Marshal(args)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

func filterTools(list []coretools.Tool, exclude ...string) []coretools.Tool {
	skip := map[string]bool{}
	for _, name := range exclude {
		skip[name] = true
	}
	out := make([]coretools.Tool, 0, len(list))
	for _, t := range list {
		if !skip[t.Name()] {
			out = append(out, t)
		}
	}
	return out
}
