package hooks

import (
	"context"
	"testing"
)

func TestSessionEndPassesReason(t *testing.T) {
	cfg := Config{SessionEnd: []Entry{{Hooks: []Hook{{Type: "command", Command: "cleanup"}}}}}
	var seen []Event
	r := NewRunner(cfg, "/tmp", "s1", WithCommandRunner(fakeRunner(
		map[string]CommandResult{"cleanup": {Stdout: "cleaned"}}, &seen,
	)))
	out, err := r.SessionEnd(context.Background(), "exit")
	if err != nil {
		t.Fatal(err)
	}
	if out != "cleaned" {
		t.Fatalf("output = %q", out)
	}
	if len(seen) != 1 || seen[0].StopReason != "exit" {
		t.Fatalf("event = %+v", seen)
	}
}

func TestPermissionRequestDecisions(t *testing.T) {
	cases := []struct {
		name   string
		result CommandResult
		want   string
	}{
		{"exit2 denies", CommandResult{Stdout: "policy says no", ExitCode: 2}, "deny"},
		{"stdout allow", CommandResult{Stdout: "allow"}, "allow"},
		{"stdout APPROVE case-insensitive", CommandResult{Stdout: "APPROVE\n"}, "allow"},
		{"stdout deny", CommandResult{Stdout: "deny"}, "deny"},
		{"no opinion", CommandResult{Stdout: ""}, ""},
		{"unrelated text defers", CommandResult{Stdout: "logged for review"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{PermissionRequest: []Entry{{Matcher: "Bash", Hooks: []Hook{{Type: "command", Command: "decide"}}}}}
			var seen []Event
			r := NewRunner(cfg, "/tmp", "s1", WithCommandRunner(fakeRunner(
				map[string]CommandResult{"decide": tc.result}, &seen,
			)))
			d, err := r.PermissionRequest(context.Background(), "Bash", map[string]any{"command": "ls"})
			if err != nil {
				t.Fatal(err)
			}
			if d.Decision != tc.want {
				t.Fatalf("decision = %q, want %q", d.Decision, tc.want)
			}
			if len(seen) != 1 || seen[0].ToolName != "Bash" || seen[0].ToolInput["command"] != "ls" {
				t.Fatalf("event = %+v", seen)
			}
		})
	}
}

func TestPermissionRequestMatcherScopes(t *testing.T) {
	cfg := Config{PermissionRequest: []Entry{{Matcher: "Bash(git *)", Hooks: []Hook{{Type: "command", Command: "ok"}}}}}
	var seen []Event
	r := NewRunner(cfg, "/tmp", "s1", WithCommandRunner(fakeRunner(
		map[string]CommandResult{"ok": {Stdout: "allow"}}, &seen,
	)))
	// Matching command pre-approved.
	d, err := r.PermissionRequest(context.Background(), "Bash", map[string]any{"command": "git push"})
	if err != nil {
		t.Fatal(err)
	}
	if d.Decision != "allow" {
		t.Fatalf("git push should be pre-approved: %+v", d)
	}
	// Non-matching command: hook not even run.
	d, err = r.PermissionRequest(context.Background(), "Bash", map[string]any{"command": "curl example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if d.Decision != "" || len(seen) != 1 {
		t.Fatalf("non-matching call should defer: %+v seen=%d", d, len(seen))
	}
}

func TestPostSamplingReceivesResponse(t *testing.T) {
	cfg := Config{PostSampling: []Entry{{Hooks: []Hook{{Type: "command", Command: "log"}}}}}
	var seen []Event
	r := NewRunner(cfg, "/tmp", "s1", WithCommandRunner(fakeRunner(nil, &seen)))
	if err := r.PostSampling(context.Background(), "the model said this"); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0].Message != "the model said this" {
		t.Fatalf("event = %+v", seen)
	}
}
