package permissions

import (
	"encoding/json"
	"os"
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
