// Package hooks ports Claude Code's hook system: shell commands that run on
// events (PreToolUse, PostToolUse, UserPromptSubmit, Stop) and can inject
// feedback or block a tool. Hooks are configured in settings.json.
package hooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"github.com/rs/zerolog"
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
}

// Hook is a single hook definition.
type Hook struct {
	Type      string `json:"type"` // currently only "command"
	Command   string `json:"command"`
	TimeoutMS int    `json:"timeout_ms,omitempty"`
}

// Entry groups hooks under a matcher.
type Entry struct {
	Matcher string `json:"matcher,omitempty"`
	Hooks   []Hook `json:"hooks"`
}

// Config holds all hook events.
type Config struct {
	PreToolUse       []Entry `json:"PreToolUse"`
	PostToolUse      []Entry `json:"PostToolUse"`
	UserPromptSubmit []Entry `json:"UserPromptSubmit"`
	Stop             []Entry `json:"Stop"`
	SessionStart     []Entry `json:"SessionStart"`
	SubagentStop     []Entry `json:"SubagentStop"`
	Notification     []Entry `json:"Notification"`
	PreCompact       []Entry `json:"PreCompact"`
	PostCompact      []Entry `json:"PostCompact"`
}

// Load reads hook configuration from a JSON settings file of the form
// {"hooks": {"PreToolUse": [...], ...}}.
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Config{}, nil
		}
		return Config{}, err
	}
	var wrapper struct {
		Hooks Config `json:"hooks"`
	}
	if err := json.Unmarshal(data, &wrapper); err != nil {
		return Config{}, fmt.Errorf("hooks: parse %s: %w", path, err)
	}
	return wrapper.Hooks, nil
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

// DefaultCommandRunner runs the command via sh -c.
func DefaultCommandRunner(ctx context.Context, command, stdin, cwd string) (CommandResult, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = cwd
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	res := CommandResult{Stdout: stdout.String(), Stderr: stderr.String()}
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			res.ExitCode = exitErr.ExitCode()
			return res, nil
		}
		return res, err
	}
	return res, nil
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

// SubagentStop runs when a subagent finishes; returns feedback (and blocked
// when a hook exits 2).
func (r *Runner) SubagentStop(ctx context.Context) (string, bool, error) {
	ev := Event{SessionID: r.sessionID, CWD: r.cwd}
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
			res, runErr := r.run(ctx, hook.Command, stdin, r.cwd)
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
