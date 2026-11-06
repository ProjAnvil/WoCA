// Package hooks implements a hook system: shell commands that run on
// events (PreToolUse, PostToolUse, UserPromptSubmit, Stop) and can inject
// feedback or block a tool. Hooks are configured in settings.json.
package hooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/projanvil/woca/internal/platform/executor"
)

// Event is the JSON payload written to a hook's stdin.
type Event struct {
	SessionID    string         `json:"session_id,omitempty"`
	CWD          string         `json:"cwd,omitempty"`
	ToolName     string         `json:"tool_name,omitempty"`
	ToolInput    map[string]any `json:"tool_input,omitempty"`
	ToolResponse string         `json:"tool_response,omitempty"`
	Prompt       string         `json:"prompt,omitempty"`
	StopReason   string         `json:"stop_reason,omitempty"`
	Message      string         `json:"message,omitempty"`
	Source       string         `json:"source,omitempty"`
	AgentName    string         `json:"agent_name,omitempty"`
}

// Hook is a single hook definition.
type Hook struct {
	Type       string `json:"type"` // currently only "command"
	Command    string `json:"command"`
	TimeoutMS  int    `json:"timeout_ms,omitempty"`
	TimeoutSec int    `json:"timeout,omitempty"` // Claude Code alias (seconds); normalized into TimeoutMS by Load
}

// Entry groups hooks under a matcher.
type Entry struct {
	Matcher string `json:"matcher,omitempty"`
	Hooks   []Hook `json:"hooks"`
	// Source names the configuration layer this entry came from (e.g. a
	// plugin name, for /hooks display). Display-only: set by loaders,
	// never parsed from user configuration.
	Source string `json:"-"`
}

// Config holds all hook events.
type Config struct {
	PreToolUse        []Entry `json:"PreToolUse"`
	PostToolUse       []Entry `json:"PostToolUse"`
	UserPromptSubmit  []Entry `json:"UserPromptSubmit"`
	Stop              []Entry `json:"Stop"`
	SessionStart      []Entry `json:"SessionStart"`
	SessionEnd        []Entry `json:"SessionEnd"`
	SubagentStart     []Entry `json:"SubagentStart"`
	SubagentStop      []Entry `json:"SubagentStop"`
	Notification      []Entry `json:"Notification"`
	PreCompact        []Entry `json:"PreCompact"`
	PostCompact       []Entry `json:"PostCompact"`
	PermissionRequest []Entry `json:"PermissionRequest"`
	PostSampling      []Entry `json:"PostSampling"`
}

// EachEvent calls fn once per event field, in canonical (struct) order,
// with the event's name and a pointer to its entries slice. fn may mutate
// through the pointer (append, replace, tag) — this is the single
// enumeration of Config's event list, so consumers (loaders, mergers,
// counters) stay correct when an event is added instead of hand-maintaining
// the 13-field list in each package.
func (c *Config) EachEvent(fn func(name string, entries *[]Entry)) {
	entryType := reflect.TypeOf(Entry{})
	v := reflect.ValueOf(c).Elem()
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" || f.Type.Kind() != reflect.Slice || f.Type.Elem() != entryType {
			continue
		}
		list, _ := v.Field(i).Addr().Interface().(*[]Entry)
		fn(f.Name, list)
	}
}

// Load reads hook configuration from a JSON settings file of the form
// {"hooks": {"PreToolUse": [...], ...}}. A missing file is an empty
// configuration, not an error. Byte-level decoding lives in Parse.
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Config{}, nil
		}
		return Config{}, err
	}
	cfg, err := Parse(data)
	if err != nil {
		return Config{}, fmt.Errorf("hooks: parse %s: %w", path, err)
	}
	return cfg, nil
}

// Parse decodes hook configuration bytes of the form
// {"hooks": {"PreToolUse": [...], ...}}, normalizing the Claude Code
// "timeout" (seconds) alias into timeout_ms. It is the byte-level
// equivalent of Load (plugin hooks files and inline manifest objects
// carry no path); Load reads the file and delegates here.
func Parse(data []byte) (Config, error) {
	var wrapper struct {
		Hooks Config `json:"hooks"`
	}
	if err := json.Unmarshal(data, &wrapper); err != nil {
		return Config{}, fmt.Errorf("invalid hooks JSON: %w", err)
	}
	wrapper.Hooks.normalizeTimeouts()
	return wrapper.Hooks, nil
}

// normalizeTimeouts resolves the Claude Code "timeout" (seconds) alias into
// TimeoutMS across every configured event: an explicit timeout_ms wins;
// otherwise timeout seconds are converted to milliseconds. Zero values keep
// the unbounded default.
func (c *Config) normalizeTimeouts() {
	c.EachEvent(func(_ string, entries *[]Entry) {
		for i := range *entries {
			hooks := (*entries)[i].Hooks
			for j := range hooks {
				if hooks[j].TimeoutMS == 0 && hooks[j].TimeoutSec > 0 {
					hooks[j].TimeoutMS = hooks[j].TimeoutSec * 1000
				}
			}
		}
	})
}

// Matcher matches a tool name (and optionally a glob over the tool input).
type Matcher struct {
	tool       *regexp.Regexp // nil = match all tool names
	argPattern *regexp.Regexp // nil = no arg filter
	argRaw     string
}

// compileMatcher builds a Matcher. A matcher may be a tool-name regex
// ("Edit|Write") or a tool-specific form "Bash(git *)" where the parenthesized
// part is a glob matched against the stringified tool input.
func compileMatcher(s string) (Matcher, error) {
	m := Matcher{}
	s = strings.TrimSpace(s)
	if s == "" || s == "*" {
		return m, nil
	}
	// Tool(arg) form.
	if idx := strings.Index(s, "("); idx > 0 && strings.HasSuffix(s, ")") {
		name := s[:idx]
		arg := s[idx+1 : len(s)-1]
		re, err := regexp.Compile("^" + regexp.QuoteMeta(name) + "$")
		if err != nil {
			return m, err
		}
		m.tool = re
		m.argRaw = arg
		argRe, err := globToRegexp(arg)
		if err != nil {
			return m, err
		}
		m.argPattern = argRe
		return m, nil
	}
	re, err := regexp.Compile("^" + s + "$")
	if err != nil {
		return m, fmt.Errorf("hooks: invalid matcher %q: %w", s, err)
	}
	m.tool = re
	return m, nil
}

// Matches reports whether the matcher applies to a tool call.
func (m Matcher) Matches(toolName string, input map[string]any) bool {
	if m.tool != nil && !m.tool.MatchString(toolName) {
		return false
	}
	if m.argPattern != nil {
		raw, _ := json.Marshal(input)
		if !m.argPattern.MatchString(string(raw)) {
			return false
		}
	}
	return true
}

// globToRegexp converts a glob (*, ?) into an unanchored regexp.
func globToRegexp(glob string) (*regexp.Regexp, error) {
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
	return regexp.Compile(b.String())
}

// CommandResult is the outcome of a hook command.
type CommandResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// CommandRunner executes a hook command with stdin. Injectable for tests.
type CommandRunner func(ctx context.Context, command, stdin, cwd string) (CommandResult, error)

// DefaultCommandRunner runs the command via sh -c on the local executor.
func DefaultCommandRunner(ctx context.Context, command, stdin, cwd string) (CommandResult, error) {
	return ExecutorCommandRunner(executor.NewLocal())(ctx, command, stdin, cwd)
}

// Decision is the outcome of a PreToolUse hook.
type Decision struct {
	Allowed  bool
	Feedback string // stdout, injected as context (or the deny message)
	Warning  string // stderr
}

// PostResult is the outcome of a PostToolUse hook.
type PostResult struct {
	Feedback string // stdout, appended to the tool result
	Blocked  bool   // exit code 2: feed stdout to the model and stop
}

// Runner executes configured hooks for a session.
type Runner struct {
	cfg       Config
	cwd       string
	sessionID string
	run       CommandRunner
	logger    zerolog.Logger
}

// RunnerOption configures a Runner.
type RunnerOption func(*Runner)

// WithCommandRunner injects a command runner (for tests).
func WithCommandRunner(r CommandRunner) RunnerOption {
	return func(x *Runner) { x.run = r }
}

// WithLogger sets the logger.
func WithLogger(l zerolog.Logger) RunnerOption {
	return func(x *Runner) { x.logger = l }
}

// NewRunner builds a hook runner.
func NewRunner(cfg Config, cwd, sessionID string, opts ...RunnerOption) *Runner {
	r := &Runner{cfg: cfg, cwd: cwd, sessionID: sessionID, run: DefaultCommandRunner, logger: zerolog.Nop()}
	for _, o := range opts {
		o(r)
	}
	return r
}

// PreToolUse runs PreToolUse hooks. Exit code 2 denies the tool.
func (r *Runner) PreToolUse(ctx context.Context, toolName string, input map[string]any) (Decision, error) {
	ev := Event{SessionID: r.sessionID, CWD: r.cwd, ToolName: toolName, ToolInput: input}
	return r.runEntries(ctx, r.cfg.PreToolUse, toolName, input, ev)
}

// PostToolUse runs PostToolUse hooks. Exit code 2 blocks continuation.
func (r *Runner) PostToolUse(ctx context.Context, toolName string, input map[string]any, response string) (PostResult, error) {
	ev := Event{SessionID: r.sessionID, CWD: r.cwd, ToolName: toolName, ToolInput: input, ToolResponse: response}
	stdout, _, blocked, err := r.runMatching(ctx, r.cfg.PostToolUse, toolName, input, ev)
	if err != nil {
		return PostResult{}, err
	}
	return PostResult{Feedback: strings.TrimSpace(stdout), Blocked: blocked}, nil
}

// UserPromptSubmit runs UserPromptSubmit hooks, returning injected context.
func (r *Runner) UserPromptSubmit(ctx context.Context, prompt string) (string, error) {
	ev := Event{SessionID: r.sessionID, CWD: r.cwd, Prompt: prompt}
	stdout, _, _, err := r.runMatching(ctx, r.cfg.UserPromptSubmit, "", nil, ev)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(stdout), nil
}

// Stop runs Stop hooks, returning feedback and whether continuation is blocked.
func (r *Runner) Stop(ctx context.Context, reason string) (string, bool, error) {
	ev := Event{SessionID: r.sessionID, CWD: r.cwd, StopReason: reason}
	stdout, _, blocked, err := r.runMatching(ctx, r.cfg.Stop, "", nil, ev)
	if err != nil {
		return "", false, err
	}
	return strings.TrimSpace(stdout), blocked, nil
}

// SessionStart runs at the start of a session.
func (r *Runner) SessionStart(ctx context.Context) (string, error) {
	ev := Event{SessionID: r.sessionID, CWD: r.cwd}
	stdout, _, _, err := r.runMatching(ctx, r.cfg.SessionStart, "", nil, ev)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(stdout), nil
}

// SubagentStart runs when a Task subagent starts (name is the resolved
// subagent name/type); returns feedback.
func (r *Runner) SubagentStart(ctx context.Context, name string) (string, error) {
	ev := Event{SessionID: r.sessionID, CWD: r.cwd, AgentName: name}
	stdout, _, _, err := r.runMatching(ctx, r.cfg.SubagentStart, "", nil, ev)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(stdout), nil
}

// SubagentStop runs when a subagent finishes; returns feedback (and blocked
// when a hook exits 2). The blocked flag is informational: woca does not
// re-enter the finished subagent (a documented divergence from Claude Code's
// continue-the-subagent semantics); the caller appends the feedback to the
// subagent report instead.
func (r *Runner) SubagentStop(ctx context.Context, name string) (string, bool, error) {
	ev := Event{SessionID: r.sessionID, CWD: r.cwd, AgentName: name}
	stdout, _, blocked, err := r.runMatching(ctx, r.cfg.SubagentStop, "", nil, ev)
	if err != nil {
		return "", false, err
	}
	return strings.TrimSpace(stdout), blocked, nil
}

// Notification runs when a notification arrives; returns feedback.
func (r *Runner) Notification(ctx context.Context, message string) (string, error) {
	ev := Event{SessionID: r.sessionID, CWD: r.cwd, Message: message}
	stdout, _, _, err := r.runMatching(ctx, r.cfg.Notification, "", nil, ev)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(stdout), nil
}

// PreCompact runs before auto-compaction; may block (exit 2).
func (r *Runner) PreCompact(ctx context.Context) (string, bool, error) {
	ev := Event{SessionID: r.sessionID, CWD: r.cwd}
	stdout, _, blocked, err := r.runMatching(ctx, r.cfg.PreCompact, "", nil, ev)
	if err != nil {
		return "", false, err
	}
	return strings.TrimSpace(stdout), blocked, nil
}

// PostCompact runs after auto-compaction.
func (r *Runner) PostCompact(ctx context.Context) (string, error) {
	ev := Event{SessionID: r.sessionID, CWD: r.cwd}
	stdout, _, _, err := r.runMatching(ctx, r.cfg.PostCompact, "", nil, ev)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(stdout), nil
}

// SessionEnd runs when a session terminates (exit, EOF, or one-shot
// completion); its output is informational only.
func (r *Runner) SessionEnd(ctx context.Context, reason string) (string, error) {
	ev := Event{SessionID: r.sessionID, CWD: r.cwd, StopReason: reason}
	stdout, _, _, err := r.runMatching(ctx, r.cfg.SessionEnd, "", nil, ev)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(stdout), nil
}

// PermissionDecision is a PermissionRequest hook's outcome: "allow" or "deny"
// decides without asking the user; "" means no opinion and the normal
// permission interrupt runs.
type PermissionDecision struct {
	Decision string
	Reason   string
}

// PermissionRequest runs before the permission interrupt, letting a hook
// pre-approve or pre-deny a tool call. Exit code 2 denies; stdout
// "allow"/"approve" allows and "deny"/"no" denies; anything else defers to
// the interactive prompt.
func (r *Runner) PermissionRequest(ctx context.Context, toolName string, input map[string]any) (PermissionDecision, error) {
	ev := Event{SessionID: r.sessionID, CWD: r.cwd, ToolName: toolName, ToolInput: input}
	stdout, _, denied, err := r.runMatching(ctx, r.cfg.PermissionRequest, toolName, input, ev)
	if err != nil {
		return PermissionDecision{}, err
	}
	if denied {
		return PermissionDecision{Decision: "deny", Reason: strings.TrimSpace(stdout)}, nil
	}
	switch strings.ToLower(strings.TrimSpace(stdout)) {
	case "allow", "approve", "yes", "y", "true":
		return PermissionDecision{Decision: "allow"}, nil
	case "deny", "no", "n", "false":
		return PermissionDecision{Decision: "deny", Reason: strings.TrimSpace(stdout)}, nil
	}
	return PermissionDecision{}, nil
}

// PostSampling runs after each model response (asynchronous analysis:
// logging, evaluation, telemetry). Callers decide whether to await it.
func (r *Runner) PostSampling(ctx context.Context, response string) error {
	ev := Event{SessionID: r.sessionID, CWD: r.cwd, Message: response}
	_, _, _, err := r.runMatching(ctx, r.cfg.PostSampling, "", nil, ev)
	return err
}

// Configured summarizes which events have hooks configured (for /doctor):
// one "Event(count)" entry per configured event, in canonical order.
func (r *Runner) Configured() []string {
	out := []string{}
	r.cfg.EachEvent(func(name string, entries *[]Entry) {
		n := 0
		for _, entry := range *entries {
			n += len(entry.Hooks)
		}
		if n > 0 {
			out = append(out, fmt.Sprintf("%s(%d)", name, n))
		}
	})
	return out
}

// runEntries runs PreToolUse semantics (deny on exit 2).
func (r *Runner) runEntries(ctx context.Context, entries []Entry, toolName string, input map[string]any, ev Event) (Decision, error) {
	stdout, stderr, denied, err := r.runMatching(ctx, entries, toolName, input, ev)
	if err != nil {
		return Decision{}, err
	}
	return Decision{Allowed: !denied, Feedback: strings.TrimSpace(stdout), Warning: strings.TrimSpace(stderr)}, nil
}

// runMatching runs all hooks whose matcher matches, combining stdout/stderr and
// tracking whether any returned exit code 2 (deny/block).
func (r *Runner) runMatching(ctx context.Context, entries []Entry, toolName string, input map[string]any, ev Event) (stdout, stderr string, blocked bool, err error) {
	evBytes, err := json.Marshal(ev)
	if err != nil {
		return "", "", false, err
	}
	stdin := string(evBytes)

	var out, errOut strings.Builder
	for _, entry := range entries {
		m, err := compileMatcher(entry.Matcher)
		if err != nil {
			return "", "", false, err
		}
		if !m.Matches(toolName, input) {
			continue
		}
		for _, hook := range entry.Hooks {
			if hook.Type != "" && hook.Type != "command" {
				r.logger.Warn().Str("type", hook.Type).Msg("unsupported hook type, skipping")
				continue
			}
			cmd := expandProjectDir(hook.Command, r.cwd)
			runCtx := ctx
			var cancel context.CancelFunc
			if hook.TimeoutMS > 0 {
				runCtx, cancel = context.WithTimeout(ctx, time.Duration(hook.TimeoutMS)*time.Millisecond)
			}
			res, runErr := r.run(runCtx, cmd, stdin, r.cwd)
			if cancel != nil {
				cancel()
			}
			if runErr != nil {
				return out.String(), errOut.String(), blocked, runErr
			}
			if res.Stdout != "" {
				out.WriteString(strings.TrimSpace(res.Stdout))
				out.WriteString("\n")
			}
			if res.Stderr != "" {
				errOut.WriteString(strings.TrimSpace(res.Stderr))
				errOut.WriteString("\n")
			}
			if res.ExitCode == 2 {
				blocked = true
			}
		}
	}
	return out.String(), errOut.String(), blocked, nil
}

// expandProjectDir substitutes ${CLAUDE_PROJECT_DIR} and ${WOCA_PROJECT_DIR}
// with the project directory. Plain text replacement — no shell evaluation.
func expandProjectDir(command, dir string) string {
	command = strings.ReplaceAll(command, "${CLAUDE_PROJECT_DIR}", dir)
	return strings.ReplaceAll(command, "${WOCA_PROJECT_DIR}", dir)
}
