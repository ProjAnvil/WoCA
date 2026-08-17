package permissions

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadErrorBranches(t *testing.T) {
	dir := t.TempDir()
	// A directory in place of the file: read error.
	if _, err := Load(dir); err == nil {
		t.Fatal("expected read error")
	}
	// Malformed JSON.
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(bad); err == nil {
		t.Fatal("expected parse error")
	}
	// Missing file: zero rules, no error.
	if r, err := Load(filepath.Join(dir, "missing.json")); err != nil || len(r.Allow) != 0 {
		t.Fatalf("missing = %+v %v", r, err)
	}
}

func TestRuleParsingAndMatching(t *testing.T) {
	r := Rules{
		Allow: []string{"Read", "Bash(git *)", "Bash(ls)"},
		Deny:  []string{"Bash(rm *)", "WebFetch"},
		Ask:   []string{"Write"},
	}
	cases := []struct {
		tool, command string
		matched       bool
		action        Action
	}{
		{"Read", "", true, ActionAllow},
		{"Bash", "git status", true, ActionAllow},
		{"Bash", "ls -la", true, ActionAllow},
		{"Bash", "rm -rf /", true, ActionDeny},
		{"WebFetch", "", true, ActionDeny},
		{"Write", "", true, ActionAsk},
		{"Grep", "", false, ""},
		{"Bash", "curl evil", false, ""},
	}
	for _, c := range cases {
		input := map[string]any{}
		if c.command != "" {
			input["command"] = c.command
		}
		got, ok := r.Action(c.tool, input)
		if !c.matched {
			if ok {
				t.Errorf("%s(%s) matched unexpectedly: %v", c.tool, c.command, got)
			}
			continue
		}
		if !ok || got != c.action {
			t.Errorf("%s(%s) = %v %v, want %v", c.tool, c.command, got, ok, c.action)
		}
	}
}

func TestRuleHelpers(t *testing.T) {
	// splitToolGlob shapes.
	for _, c := range []struct {
		rule       string
		name, glob string
		ok         bool
	}{
		{"Bash(git *)", "Bash", "git *", true},
		{"Read", "", "", false},
		{"(x)", "", "", false},
		{"Bad-Name(x)", "", "", false},
		{"Bash(unclosed", "", "", false},
	} {
		name, glob, ok := splitToolGlob(c.rule)
		if ok != c.ok || (ok && (name != c.name || glob != c.glob)) {
			t.Errorf("splitToolGlob(%q) = %q %q %v", c.rule, name, glob, ok)
		}
	}
	// isToolName accepts letters/digits (digit-first rejected).
	for _, c := range []struct {
		s    string
		want bool
	}{
		{"Bash", true},
		{"WebFetch2", true},
		{"", false},
		{"2Bash", false},
		{"Bad-Name", false},
	} {
		if got := isToolName(c.s); got != c.want {
			t.Errorf("isToolName(%q) = %v", c.s, got)
		}
	}
	// globMatch with an invalid pattern degrades to false.
	if globMatch("[", "x") {
		t.Error("invalid glob should not match")
	}
	if !globMatch("git *", "git status") {
		t.Error("glob should match")
	}
}
