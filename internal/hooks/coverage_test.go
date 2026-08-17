package hooks

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

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
