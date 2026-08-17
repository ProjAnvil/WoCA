package hooks

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

// fakeRunner returns a canned result, recording the received event.
func fakeRunner(results map[string]CommandResult, seen *[]Event) CommandRunner {
	return func(ctx context.Context, command, stdin, cwd string) (CommandResult, error) {
		var ev Event
		_ = json.Unmarshal([]byte(stdin), &ev)
		*seen = append(*seen, ev)
		if r, ok := results[command]; ok {
			return r, nil
		}
		return CommandResult{}, nil
	}
}

func TestMatcherToolNameRegex(t *testing.T) {
	m, err := compileMatcher("Edit|Write")
	if err != nil {
		t.Fatal(err)
	}
	if !m.Matches("Edit", nil) || !m.Matches("Write", nil) {
		t.Error("Edit|Write should match Edit and Write")
	}
	if m.Matches("Bash", nil) {
		t.Error("Edit|Write should not match Bash")
	}
}

func TestMatcherToolSpecificArg(t *testing.T) {
	m, err := compileMatcher("Bash(git *)")
	if err != nil {
		t.Fatal(err)
	}
	if !m.Matches("Bash", map[string]any{"command": "git status"}) {
		t.Error("Bash(git *) should match a git command")
	}
	if m.Matches("Bash", map[string]any{"command": "rm -rf /"}) {
		t.Error("Bash(git *) should not match rm")
	}
	if m.Matches("Read", map[string]any{"command": "git status"}) {
		t.Error("Bash(git *) should not match the Read tool")
	}
}

func TestPreToolUseDeny(t *testing.T) {
	cfg := Config{PreToolUse: []Entry{{Matcher: "Bash", Hooks: []Hook{{Type: "command", Command: "deny"}}}}}
	var seen []Event
	r := NewRunner(cfg, "/tmp", "s1", WithCommandRunner(fakeRunner(
		map[string]CommandResult{"deny": {Stdout: "blocked by policy", ExitCode: 2}},
		&seen,
	)))
	d, err := r.PreToolUse(context.Background(), "Bash", map[string]any{"command": "rm"})
	if err != nil {
		t.Fatal(err)
	}
	if d.Allowed {
		t.Fatal("expected deny")
	}
	if !strings.Contains(d.Feedback, "blocked by policy") {
		t.Fatalf("feedback = %q", d.Feedback)
	}
	if len(seen) != 1 || seen[0].ToolName != "Bash" {
		t.Fatalf("event = %+v", seen)
	}
}

func TestPreToolUseAllowFeedback(t *testing.T) {
	cfg := Config{PreToolUse: []Entry{{Matcher: "Read", Hooks: []Hook{{Type: "command", Command: "ok"}}}}}
	var seen []Event
	r := NewRunner(cfg, "/tmp", "s1", WithCommandRunner(fakeRunner(
		map[string]CommandResult{"ok": {Stdout: "extra context", ExitCode: 0}},
		&seen,
	)))
	d, err := r.PreToolUse(context.Background(), "Read", map[string]any{"file_path": "x"})
	if err != nil {
		t.Fatal(err)
	}
	if !d.Allowed || d.Feedback != "extra context" {
		t.Fatalf("decision = %+v", d)
	}
}

func TestPostToolUseBlock(t *testing.T) {
	cfg := Config{PostToolUse: []Entry{{Matcher: "Bash", Hooks: []Hook{{Command: "post"}}}}}
	var seen []Event
	r := NewRunner(cfg, "/tmp", "s1", WithCommandRunner(fakeRunner(
		map[string]CommandResult{"post": {Stdout: "please stop", ExitCode: 2}},
		&seen,
	)))
	p, err := r.PostToolUse(context.Background(), "Bash", map[string]any{"command": "x"}, "output")
	if err != nil {
		t.Fatal(err)
	}
	if !p.Blocked || p.Feedback != "please stop" {
		t.Fatalf("post = %+v", p)
	}
	if seen[0].ToolResponse != "output" {
		t.Fatalf("event tool_response = %q", seen[0].ToolResponse)
	}
}

func TestStopHook(t *testing.T) {
	cfg := Config{Stop: []Entry{{Matcher: "*", Hooks: []Hook{{Command: "stop"}}}}}
	r := NewRunner(cfg, "/tmp", "s1", WithCommandRunner(fakeRunner(
		map[string]CommandResult{"stop": {Stdout: "do more", ExitCode: 0}},
		&[]Event{},
	)))
	feedback, blocked, err := r.Stop(context.Background(), "completed")
	if err != nil {
		t.Fatal(err)
	}
	if feedback != "do more" || blocked {
		t.Fatalf("stop = %q blocked=%v", feedback, blocked)
	}
}

func TestSessionStartHook(t *testing.T) {
	cfg := Config{SessionStart: []Entry{{Matcher: "*", Hooks: []Hook{{Command: "start"}}}}}
	var seen []Event
	r := NewRunner(cfg, "/tmp", "s1", WithCommandRunner(fakeRunner(
		map[string]CommandResult{"start": {Stdout: "  hello session  \n", ExitCode: 0}},
		&seen,
	)))
	out, err := r.SessionStart(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if out != "hello session" {
		t.Fatalf("out = %q", out)
	}
	if len(seen) != 1 {
		t.Fatalf("expected 1 event, got %d", len(seen))
	}
}

func TestSubagentStopBlock(t *testing.T) {
	cfg := Config{SubagentStop: []Entry{{Matcher: "*", Hooks: []Hook{{Command: "sub"}}}}}
	var seen []Event
	r := NewRunner(cfg, "/tmp", "s1", WithCommandRunner(fakeRunner(
		map[string]CommandResult{"sub": {Stdout: "stop subagent", ExitCode: 2}},
		&seen,
	)))
	out, blocked, err := r.SubagentStop(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !blocked || out != "stop subagent" {
		t.Fatalf("out = %q blocked = %v", out, blocked)
	}
}

func TestNotificationPassesMessage(t *testing.T) {
	cfg := Config{Notification: []Entry{{Matcher: "*", Hooks: []Hook{{Command: "notify"}}}}}
	var seen []Event
	r := NewRunner(cfg, "/tmp", "s1", WithCommandRunner(fakeRunner(
		map[string]CommandResult{"notify": {Stdout: "ack", ExitCode: 0}},
		&seen,
	)))
	out, err := r.Notification(context.Background(), "hello there")
	if err != nil {
		t.Fatal(err)
	}
	if out != "ack" {
		t.Fatalf("out = %q", out)
	}
	if len(seen) != 1 || seen[0].Message != "hello there" {
		t.Fatalf("event = %+v", seen)
	}
}

func TestPreCompactBlock(t *testing.T) {
	cfg := Config{PreCompact: []Entry{{Matcher: "*", Hooks: []Hook{{Command: "pre"}}}}}
	var seen []Event
	r := NewRunner(cfg, "/tmp", "s1", WithCommandRunner(fakeRunner(
		map[string]CommandResult{"pre": {Stdout: "too big", ExitCode: 2}},
		&seen,
	)))
	out, blocked, err := r.PreCompact(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !blocked || out != "too big" {
		t.Fatalf("out = %q blocked = %v", out, blocked)
	}
}

func TestPostCompactHook(t *testing.T) {
	cfg := Config{PostCompact: []Entry{{Matcher: "*", Hooks: []Hook{{Command: "postc"}}}}}
	var seen []Event
	r := NewRunner(cfg, "/tmp", "s1", WithCommandRunner(fakeRunner(
		map[string]CommandResult{"postc": {Stdout: "  compacted  ", ExitCode: 0}},
		&seen,
	)))
	out, err := r.PostCompact(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if out != "compacted" {
		t.Fatalf("out = %q", out)
	}
}

func TestNonToolEventOnlyWildcardMatcher(t *testing.T) {
	cfg := Config{SessionStart: []Entry{
		{Matcher: "Bash", Hooks: []Hook{{Command: "never"}}},
		{Matcher: "*", Hooks: []Hook{{Command: "always"}}},
	}}
	var seen []Event
	r := NewRunner(cfg, "/tmp", "s1", WithCommandRunner(fakeRunner(
		map[string]CommandResult{"always": {Stdout: "ran", ExitCode: 0}},
		&seen,
	)))
	out, err := r.SessionStart(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if out != "ran" {
		t.Fatalf("out = %q", out)
	}
	if len(seen) != 1 {
		t.Fatalf("expected 1 event, got %d", len(seen))
	}
}

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	content := `{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"echo hi"}]}]}}`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.PreToolUse) != 1 || cfg.PreToolUse[0].Hooks[0].Command != "echo hi" {
		t.Fatalf("cfg = %+v", cfg)
	}
}

func TestLoadConfigMissingFile(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.PreToolUse) != 0 {
		t.Fatalf("expected empty config, got %+v", cfg)
	}
}

func TestDefaultCommandRunner(t *testing.T) {
	res, err := DefaultCommandRunner(context.Background(), "echo out; echo err 1>&2", "", "")
	if err != nil || res.Stdout == "" || res.Stderr == "" {
		t.Fatalf("res = %+v %v", res, err)
	}
	// Exit codes are captured, not errors.
	res, err = DefaultCommandRunner(context.Background(), "exit 5", "", "")
	if err != nil || res.ExitCode != 5 {
		t.Fatalf("exit = %+v %v", res, err)
	}
	// A cancelled context surfaces as an error.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := DefaultCommandRunner(ctx, "sleep 5", "", ""); err == nil {
		t.Fatal("expected ctx error")
	}
}

func mkRunner(cfg Config) *Runner {
	return NewRunner(cfg, ".", "sess", WithLogger(zerolog.Nop()))
}

func hookCmd(event string, command string) Config {
	return Config{
		PreToolUse:        []Entry{{Hooks: []Hook{{Type: "command", Command: command}}}},
		PostToolUse:       []Entry{{Hooks: []Hook{{Type: "command", Command: command}}}},
		UserPromptSubmit:  []Entry{{Hooks: []Hook{{Type: "command", Command: command}}}},
		Stop:              []Entry{{Hooks: []Hook{{Type: "command", Command: command}}}},
		SessionStart:      []Entry{{Hooks: []Hook{{Type: "command", Command: command}}}},
		SessionEnd:        []Entry{{Hooks: []Hook{{Type: "command", Command: command}}}},
		SubagentStop:      []Entry{{Hooks: []Hook{{Type: "command", Command: command}}}},
		Notification:      []Entry{{Hooks: []Hook{{Type: "command", Command: command}}}},
		PreCompact:        []Entry{{Hooks: []Hook{{Type: "command", Command: command}}}},
		PostCompact:       []Entry{{Hooks: []Hook{{Type: "command", Command: command}}}},
		PermissionRequest: []Entry{{Hooks: []Hook{{Type: "command", Command: command}}}},
		PostSampling:      []Entry{{Hooks: []Hook{{Type: "command", Command: command}}}},
	}
}

func TestAllHookEventsHappyAndError(t *testing.T) {
	ctx := context.Background()
	ok := mkRunner(hookCmd("", "echo feedback"))
	// The sh -c of a missing binary only yields exit code 127; a genuine
	// runner error needs injection.
	fail := NewRunner(hookCmd("", "echo x"), ".", "sess",
		WithLogger(zerolog.Nop()),
		WithCommandRunner(func(ctx context.Context, command, stdin, cwd string) (CommandResult, error) {
			return CommandResult{}, errors.New("runner broke")
		}))

	if d, err := ok.PreToolUse(ctx, "Bash", nil); err != nil || !d.Allowed {
		t.Fatalf("pre ok = %+v %v", d, err)
	}
	if _, err := fail.PreToolUse(ctx, "Bash", nil); err == nil {
		t.Fatal("pre fail")
	}
	if p, err := ok.PostToolUse(ctx, "Bash", nil, "res"); err != nil || p.Feedback == "" {
		t.Fatalf("post ok = %+v %v", p, err)
	}
	if _, err := fail.PostToolUse(ctx, "Bash", nil, "res"); err == nil {
		t.Fatal("post fail")
	}
	if fb, err := ok.UserPromptSubmit(ctx, "prompt"); err != nil || fb != "feedback" {
		t.Fatalf("ups ok = %q %v", fb, err)
	}
	if _, err := fail.UserPromptSubmit(ctx, "prompt"); err == nil {
		t.Fatal("ups fail")
	}
	if fb, blocked, err := ok.Stop(ctx, "end"); err != nil || fb != "feedback" || blocked {
		t.Fatalf("stop ok = %q %v %v", fb, blocked, err)
	}
	if _, _, err := fail.Stop(ctx, "end"); err == nil {
		t.Fatal("stop fail")
	}
	if fb, err := ok.SessionStart(ctx); err != nil || fb != "feedback" {
		t.Fatalf("start ok = %q %v", fb, err)
	}
	if _, err := fail.SessionStart(ctx); err == nil {
		t.Fatal("start fail")
	}
	if fb, err := ok.SessionEnd(ctx, "exit"); err != nil || fb != "feedback" {
		t.Fatalf("end ok = %q %v", fb, err)
	}
	if _, err := fail.SessionEnd(ctx, "exit"); err == nil {
		t.Fatal("end fail")
	}
	if fb, blocked, err := ok.SubagentStop(ctx); err != nil || fb != "feedback" || blocked {
		t.Fatalf("substop ok = %q %v %v", fb, blocked, err)
	}
	if _, _, err := fail.SubagentStop(ctx); err == nil {
		t.Fatal("substop fail")
	}
	if fb, err := ok.Notification(ctx, "tool needs approval"); err != nil || fb != "feedback" {
		t.Fatalf("notify ok = %q %v", fb, err)
	}
	if _, err := fail.Notification(ctx, "x"); err == nil {
		t.Fatal("notify fail")
	}
	if fb, blocked, err := ok.PreCompact(ctx); err != nil || fb != "feedback" || blocked {
		t.Fatalf("precompact ok = %q %v %v", fb, blocked, err)
	}
	if _, _, err := fail.PreCompact(ctx); err == nil {
		t.Fatal("precompact fail")
	}
	if fb, err := ok.PostCompact(ctx); err != nil || fb != "feedback" {
		t.Fatalf("postcompact ok = %q %v", fb, err)
	}
	if _, err := fail.PostCompact(ctx); err == nil {
		t.Fatal("postcompact fail")
	}
	if _, err := ok.PermissionRequest(ctx, "Bash", nil); err != nil {
		t.Fatalf("perm ok: %v", err)
	}
	if _, err := fail.PermissionRequest(ctx, "Bash", nil); err == nil {
		t.Fatal("perm fail")
	}
	if err := ok.PostSampling(ctx, "resp"); err != nil {
		t.Fatalf("sampling ok = %v", err)
	}
	if err := fail.PostSampling(ctx, "resp"); err == nil {
		t.Fatal("sampling fail")
	}
}

func TestConfiguredSummary(t *testing.T) {
	empty := mkRunner(Config{})
	if got := empty.Configured(); len(got) != 0 {
		t.Fatalf("empty = %v", got)
	}
	h := []Hook{{Type: "command", Command: "echo x"}}
	cfg := Config{
		PreToolUse: []Entry{{Hooks: h}, {Hooks: h}},
		Stop:       []Entry{{Hooks: h}},
	}
	r := mkRunner(cfg)
	got := r.Configured()
	if len(got) != 2 || !strings.HasPrefix(got[0], "PreToolUse(2)") {
		t.Fatalf("Configured = %v", got)
	}
}

func TestUnsupportedHookTypeSkipped(t *testing.T) {
	cfg := Config{
		PreToolUse: []Entry{{Hooks: []Hook{
			{Type: "lua", Command: "echo x"},
			{Type: "command", Command: "echo kept"},
		}}},
	}
	r := mkRunner(cfg)
	d, err := r.PreToolUse(context.Background(), "Bash", nil)
	if err != nil || !strings.Contains(d.Feedback, "kept") {
		t.Fatalf("decision = %+v %v", d, err)
	}
}

func TestMatcherCompileErrors(t *testing.T) {
	if _, err := compileMatcher("[invalid"); err == nil {
		t.Fatal("expected matcher compile error")
	}
	if _, err := compileMatcher("Bash([bad"); err == nil {
		t.Fatal("expected arg-pattern compile error")
	}
	if _, err := globToRegexp(""); err != nil {
		t.Fatalf("empty glob: %v", err)
	}
	_ = errors.New
}
