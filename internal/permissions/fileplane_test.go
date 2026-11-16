package permissions

// File-plane rules (deepagents borrow): "Tool(glob)" rules on the file tools
// match the call's PATH argument — not the JSON-serialized input — and the
// virtual FileWrite/FileRead names cover the write/read operation classes.
// Globs are path-shaped: "*", "?" and "[...]" stay inside a segment, "**"
// spans whole segments.

import (
	"testing"
)

func TestFilePlaneRules(t *testing.T) {
	cases := []struct {
		name  string
		rules Rules
		tool  string
		args  map[string]any
		want  Action
	}{
		{
			name:  "edit dot-env anywhere",
			rules: Rules{Deny: []string{"Edit(**/.env)"}},
			tool:  "Edit",
			args:  map[string]any{"file_path": "config/.env", "old_string": "a", "new_string": "b"},
			want:  ActionDeny,
		},
		{
			name:  "root file needs the zero-segment ** too",
			rules: Rules{Deny: []string{"Edit(**/.env)"}},
			tool:  "Edit",
			args:  map[string]any{"file_path": ".env", "old_string": "a", "new_string": "b"},
			want:  ActionDeny,
		},
		{
			name:  "other paths untouched",
			rules: Rules{Deny: []string{"Edit(**/.env)"}},
			tool:  "Edit",
			args:  map[string]any{"file_path": "src/main.go", "old_string": "a", "new_string": "b"},
			want:  "",
		},
		{
			name:  "filewrite plane covers write edit notebook",
			rules: Rules{Ask: []string{"FileWrite(secrets/**)"}},
			tool:  "Edit",
			args:  map[string]any{"file_path": "secrets/creds.txt", "old_string": "a", "new_string": "b"},
			want:  ActionAsk,
		},
		{
			name:  "filewrite plane covers plain write",
			rules: Rules{Ask: []string{"FileWrite(secrets/**)"}},
			tool:  "Write",
			args:  map[string]any{"file_path": "secrets/new.txt", "content": "x"},
			want:  ActionAsk,
		},
		{
			name:  "filewrite plane covers notebook edit",
			rules: Rules{Ask: []string{"FileWrite(secrets/**)"}},
			tool:  "NotebookEdit",
			args:  map[string]any{"notebook_path": "secrets/nb.ipynb", "new_source": "x"},
			want:  ActionAsk,
		},
		{
			name:  "filewrite plane excludes reads",
			rules: Rules{Ask: []string{"FileWrite(secrets/**)"}},
			tool:  "Read",
			args:  map[string]any{"file_path": "secrets/creds.txt"},
			want:  "",
		},
		{
			name:  "fileread plane covers search roots",
			rules: Rules{Deny: []string{"FileRead(**/keys/**)"}},
			tool:  "Grep",
			args:  map[string]any{"pattern": "x", "path": "keys"},
			want:  ActionDeny,
		},
		{
			name:  "star stays inside a segment",
			rules: Rules{Allow: []string{"FileWrite(*.go)"}},
			tool:  "Write",
			args:  map[string]any{"file_path": "a.go", "content": "x"},
			want:  ActionAllow,
		},
		{
			name:  "star does not cross separators",
			rules: Rules{Allow: []string{"FileWrite(*.go)"}},
			tool:  "Write",
			args:  map[string]any{"file_path": "src/a.go", "content": "x"},
			want:  "",
		},
		{
			name:  "character class",
			rules: Rules{Allow: []string{"FileWrite(src/[ab]/**)"}},
			tool:  "Write",
			args:  map[string]any{"file_path": "src/b/x.txt", "content": "x"},
			want:  ActionAllow,
		},
		{
			name:  "paths are cleaned before matching",
			rules: Rules{Allow: []string{"FileWrite(docs/**)"}},
			tool:  "Write",
			args:  map[string]any{"file_path": "./docs//nested/x.md", "content": "x"},
			want:  ActionAllow,
		},
		{
			name:  "missing path argument never matches",
			rules: Rules{Deny: []string{"FileWrite(**/.env)"}},
			tool:  "Write",
			args:  map[string]any{"content": "x"},
			want:  "",
		},
		{
			name:  "bash keeps the json form",
			rules: Rules{Allow: []string{"Bash(git *)"}},
			tool:  "Bash",
			args:  map[string]any{"command": "git push"},
			want:  ActionAllow,
		},
		{
			name:  "file rule does not leak to bash",
			rules: Rules{Deny: []string{"FileWrite(**.sh)"}},
			tool:  "Bash",
			args:  map[string]any{"command": "echo x.sh"},
			want:  "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := tc.rules.Action(tc.tool, tc.args)
			if !ok && tc.want != "" {
				t.Fatalf("Action(%s) = no match, want %q", tc.tool, tc.want)
			}
			if ok && got != tc.want {
				t.Fatalf("Action(%s) = %q, want %q", tc.tool, got, tc.want)
			}
		})
	}
}

func TestPathGlobMatch(t *testing.T) {
	cases := []struct {
		glob, path string
		want       bool
	}{
		{"**", "anything/at/all", true},
		{"**", "", true},          // zero segments is a match for bare **
		{"a/**", "a", true},       // ** spans zero segments
		{"a/**", "a/b/c", true},   // and many
		{"a/**", "b/a", false},    // anchored at the whole path
		{"**/x.go", "x.go", true}, // zero directories
		{"**/x.go", "deep/nest/x.go", true},
		{"src/*.go", "src/a.go", true},
		{"src/*.go", "src/sub/a.go", false},
		{"src/*.go", "src/a.txt", false},
		{"?.go", "a.go", true},
		{"?.go", "ab.go", false},
		{"[a-c].go", "b.go", true},
		{"[!a-c].go", "d.go", true},
		{"[!a-c].go", "b.go", false},
	}
	for _, tc := range cases {
		if got := pathGlobMatch(tc.glob, tc.path); got != tc.want {
			t.Errorf("pathGlobMatch(%q, %q) = %v, want %v", tc.glob, tc.path, got, tc.want)
		}
	}
}
