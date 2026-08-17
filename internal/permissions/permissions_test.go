package permissions

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// wantAction asserts the effective action for a tool call.
func wantAction(t *testing.T, r Rules, tool string, input map[string]any, want Action, wantOK bool) {
	t.Helper()
	got, ok := r.Action(tool, input)
	if ok != wantOK || got != want {
		t.Errorf("Action(%q) = (%q, %v), want (%q, %v)", tool, got, ok, want, wantOK)
	}
}

func TestLoadMissingFile(t *testing.T) {
	got, err := Load(t.TempDir() + "/does-not-exist.json")
	if err != nil {
		t.Fatalf("Load missing file: unexpected error %v", err)
	}
	if got.Allow != nil || got.Deny != nil || got.Ask != nil {
		t.Fatalf("Load missing file: got %+v, want zero Rules", got)
	}
}

func TestLoadValidFile(t *testing.T) {
	path := t.TempDir() + "/settings.json"
	content := `{"permissions":{"allow":["Read","Glob"],"deny":["Bash"],"ask":["Edit|Write"]}}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load valid file: %v", err)
	}

	want := Rules{
		Allow: []string{"Read", "Glob"},
		Deny:  []string{"Bash"},
		Ask:   []string{"Edit|Write"},
	}
	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(want)
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("Load valid file: got %s, want %s", gotJSON, wantJSON)
	}
}

func TestLoadInvalidJSON(t *testing.T) {
	path := t.TempDir() + "/settings.json"
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := Load(path); err == nil {
		t.Fatalf("Load invalid JSON: expected error, got nil")
	}
}

func TestActionPrecedence(t *testing.T) {
	// deny beats ask beats allow.
	r := Rules{
		Deny:  []string{"Bash"},
		Ask:   []string{"Bash"},
		Allow: []string{"Bash"},
	}
	wantAction(t, r, "Bash", nil, ActionDeny, true)

	// ask beats allow.
	r = Rules{
		Ask:   []string{"Read"},
		Allow: []string{"Read"},
	}
	wantAction(t, r, "Read", nil, ActionAsk, true)

	// allow alone.
	r = Rules{Allow: []string{"Read"}}
	wantAction(t, r, "Read", nil, ActionAllow, true)
}

func TestActionExactMatch(t *testing.T) {
	r := Rules{Allow: []string{"Bash", "Read"}}
	wantAction(t, r, "Bash", nil, ActionAllow, true)
	wantAction(t, r, "Read", nil, ActionAllow, true)
	wantAction(t, r, "Write", nil, "", false)
}

func TestActionRegexMatch(t *testing.T) {
	r := Rules{Allow: []string{"Edit|Write"}}
	wantAction(t, r, "Edit", nil, ActionAllow, true)
	wantAction(t, r, "Write", nil, ActionAllow, true)
	wantAction(t, r, "Read", nil, "", false)
}

func TestActionToolGlob(t *testing.T) {
	r := Rules{Allow: []string{"Bash(git *)"}}
	wantAction(t, r, "Bash", map[string]any{"command": "git status"}, ActionAllow, true)
	// Non-matching command under the same tool.
	wantAction(t, r, "Bash", map[string]any{"command": "ls -la"}, "", false)
	// Wrong tool name.
	wantAction(t, r, "Read", map[string]any{"command": "git status"}, "", false)
}

func TestActionToolGlobQuestionMark(t *testing.T) {
	r := Rules{Allow: []string{"Bash(ls -?)"}}
	wantAction(t, r, "Bash", map[string]any{"command": "ls -a"}, ActionAllow, true)
	wantAction(t, r, "Bash", map[string]any{"command": "pwd"}, "", false)
}

func TestActionNoMatch(t *testing.T) {
	var r Rules
	wantAction(t, r, "Bash", nil, "", false)

	r = Rules{Allow: []string{"Read"}}
	wantAction(t, r, "Bash", nil, "", false)
}

func FuzzAction(f *testing.F) {
	f.Add("Bash", "git status")
	f.Add("Read", "")
	f.Fuzz(func(t *testing.T, tool, arg string) {
		r := Rules{Deny: []string{"Bash(rm *)"}, Allow: []string{"Bash(git *)", "Read"}}
		_, _ = r.Action(tool, map[string]any{"command": arg}) // must not panic
	})
}

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
