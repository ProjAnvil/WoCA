package hooks

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
