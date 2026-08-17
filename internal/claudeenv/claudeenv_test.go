package claudeenv

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// setHome redirects homeDir to a temp tree and returns its root.
func setHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	orig := homeDir
	homeDir = func() (string, error) { return home, nil }
	t.Cleanup(func() { homeDir = orig })
	return home
}

func writeSkill(t *testing.T, home, name, frontmatter string) {
	t.Helper()
	dir := filepath.Join(home, ".claude", "skills", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "# " + name + "\n"
	if frontmatter != "" {
		body = frontmatter + "\n" + body
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSkillsDetection(t *testing.T) {
	home := setHome(t)
	writeSkill(t, home, "alpha", "---\nname: alpha\ndescription: does alpha things\n---")
	writeSkill(t, home, "beta", "---\ndescription: beta desc\n---")
	// A dir without SKILL.md is ignored.
	if err := os.MkdirAll(filepath.Join(home, ".claude", "skills", "noskill"), 0o755); err != nil {
		t.Fatal(err)
	}

	list, err := Skills()
	if err != nil {
		t.Fatalf("Skills: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("skills = %+v, want 2", list)
	}
	if list[0].Name != "alpha" || list[0].Description != "does alpha things" {
		t.Errorf("alpha = %+v", list[0])
	}
	if list[1].Name != "beta" || list[1].Description != "beta desc" {
		t.Errorf("beta = %+v", list[1])
	}
}

func TestSyncSkillsLinksAndUnlinks(t *testing.T) {
	home := setHome(t)
	writeSkill(t, home, "alpha", "")
	writeSkill(t, home, "beta", "")

	woca := filepath.Join(home, ".woca")
	linked, err := SyncSkills(woca, []string{"alpha"})
	if err != nil {
		t.Fatalf("SyncSkills: %v", err)
	}
	if len(linked) != 1 || linked[0] != "alpha" {
		t.Fatalf("linked = %v", linked)
	}
	link := filepath.Join(woca, "skills", "alpha")
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("expected symlink at %s: %v", link, err)
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(woca, "skills", target)
	}
	if want := filepath.Join(home, ".claude", "skills", "alpha"); target != want {
		t.Errorf("symlink -> %s, want %s", target, want)
	}

	// A real directory in ~/.woca/skills is never touched.
	own := filepath.Join(woca, "skills", "mine")
	if err := os.MkdirAll(own, 0o755); err != nil {
		t.Fatal(err)
	}
	// Switching the selection removes the stale managed link and adds beta.
	if _, err := SyncSkills(woca, []string{"beta"}); err != nil {
		t.Fatalf("SyncSkills(2): %v", err)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Errorf("alpha link should be removed after deselection (err=%v)", err)
	}
	if _, err := os.Lstat(filepath.Join(woca, "skills", "beta")); err != nil {
		t.Errorf("beta link missing: %v", err)
	}
	if _, err := os.Stat(own); err != nil {
		t.Errorf("real skill dir must not be removed: %v", err)
	}
}

func writeClaudeJSON(t *testing.T, home string, servers map[string]any) {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"otherKey":   1,
		"mcpServers": servers,
		"projects":   map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestServersAndSpecs(t *testing.T) {
	home := setHome(t)
	writeClaudeJSON(t, home, map[string]any{
		"deepwiki": map[string]any{"type": "http", "url": "https://mcp.deepwiki.com/mcp"},
		"context7": map[string]any{"command": "npx", "args": []string{"-y", "@context7/mcp"},
			"env": map[string]string{"API_KEY": "s3cret"}},
	})
	list, err := Servers()
	if err != nil {
		t.Fatalf("Servers: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("servers = %+v, want 2", list)
	}
	dw, c7 := byName(t, list, "deepwiki"), byName(t, list, "context7")
	if dw.Type != "http" || dw.URL != "https://mcp.deepwiki.com/mcp" {
		t.Errorf("deepwiki = %+v", dw)
	}
	if c7.Type != "stdio" || !c7.HasEnv {
		t.Errorf("context7 = %+v", c7)
	}
	if c7.Command == "" || containsPlain(c7.Command, "s3cret") {
		t.Errorf("context7 display command must be env-free, got %q", c7.Command)
	}
	if got := dw.Spec(); got != "deepwiki=https://mcp.deepwiki.com/mcp" {
		t.Errorf("deepwiki spec = %q", got)
	}
	spec := c7.Spec()
	if !containsPlain(spec, "API_KEY=s3cret") || !containsPlain(spec, "npx -y @context7/mcp") {
		t.Errorf("context7 spec = %q", spec)
	}
	if containsPlain(spec, "'s3cret'") || containsPlain(spec, "'@") {
		t.Errorf("context7 spec over-quotes: %q", spec)
	}
}

func byName(t *testing.T, list []MCPServer, name string) MCPServer {
	t.Helper()
	for _, s := range list {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("server %s not in %v", name, list)
	return MCPServer{}
}

func containsPlain(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && index(s, sub) >= 0)
}

func index(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestMCPConfigRoundTrip(t *testing.T) {
	home := setHome(t)
	writeClaudeJSON(t, home, map[string]any{
		"deepwiki": map[string]any{"type": "http", "url": "https://mcp.deepwiki.com/mcp"},
	})
	list, err := Servers()
	if err != nil {
		t.Fatal(err)
	}
	woca := filepath.Join(home, ".woca")
	if err := WriteMCPConfig(woca, list); err != nil {
		t.Fatalf("WriteMCPConfig: %v", err)
	}
	// The file must exist with the selected server, secrets aside.
	data, err := os.ReadFile(filepath.Join(woca, "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc["mcpServers"].(map[string]any)["deepwiki"]; !ok {
		t.Fatalf("mcp.json missing deepwiki: %s", data)
	}
	specs, err := LoadMCPConfig(woca)
	if err != nil {
		t.Fatalf("LoadMCPConfig: %v", err)
	}
	if len(specs) != 1 || specs[0] != "deepwiki=https://mcp.deepwiki.com/mcp" {
		t.Fatalf("specs = %v", specs)
	}
}

func TestServersMissingFile(t *testing.T) {
	setHome(t)
	list, err := Servers()
	if err != nil || list != nil {
		t.Errorf("Servers() on missing file = %+v, %v; want nil, nil", list, err)
	}
}
