package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/rs/zerolog"

	"github.com/projanvil/woca/internal/settings"
)

// newClaudeEnvServer builds a server with a temp settings store and the
// optional ApplyClaudeEnv hook.
func newClaudeEnvServer(t *testing.T, apply func(skills, mcp []string) error) (*Server, *httptest.Server) {
	t.Helper()
	st, err := settings.Open(t.TempDir() + "/s.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	s := New(Config{
		Static:         fstest.MapFS{},
		Logger:         zerolog.Nop(),
		Settings:       st,
		ApplyClaudeEnv: apply,
	})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts
}

func TestClaudeEnvGet(t *testing.T) {
	s, ts := newClaudeEnvServer(t, nil)

	// Persist a selection through the settings store (the read side of the
	// checkbox page reports it back verbatim).
	cfg := s.settings.Load()
	cfg.ClaudeSkills = []string{"code-review"}
	cfg.ClaudeMCP = []string{"fetch"}
	if err := s.settings.Save(cfg); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Get(ts.URL + "/api/claudeenv")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d", resp.StatusCode)
	}
	var out struct {
		Skills   []json.RawMessage `json:"skills"`
		MCP      []json.RawMessage `json:"mcp"`
		Selected struct {
			Skills []string `json:"skills"`
			MCP    []string `json:"mcp"`
		} `json:"selected"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	// Detection is best-effort over ~/.claude: the arrays must exist (never
	// null) even when nothing was detected.
	if out.Skills == nil || out.MCP == nil {
		t.Fatalf("skills/mcp lists must be non-null: %+v", out)
	}
	if len(out.Selected.Skills) != 1 || out.Selected.Skills[0] != "code-review" {
		t.Fatalf("selected skills = %v", out.Selected.Skills)
	}
	if len(out.Selected.MCP) != 1 || out.Selected.MCP[0] != "fetch" {
		t.Fatalf("selected mcp = %v", out.Selected.MCP)
	}
}

func TestClaudeEnvGetWithoutSettings(t *testing.T) {
	// No settings store: the selection degrades to empty arrays, not null.
	s := New(Config{Static: fstest.MapFS{}, Logger: zerolog.Nop()})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	if sk, mcp := s.currentClaudeSelection(); sk != nil || mcp != nil {
		t.Fatalf("currentClaudeSelection without settings = %v %v, want nils", sk, mcp)
	}
	resp, err := http.Get(ts.URL + "/api/claudeenv")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Selected struct {
			Skills []string `json:"skills"`
			MCP    []string `json:"mcp"`
		} `json:"selected"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Selected.Skills == nil || out.Selected.MCP == nil {
		t.Fatalf("selection must be non-null: %+v", out.Selected)
	}
}

func TestClaudeEnvPut(t *testing.T) {
	// Without the apply hook the write side is unavailable.
	_, ts := newClaudeEnvServer(t, nil)
	resp, err := http.NewRequest(http.MethodPut, ts.URL+"/api/claudeenv", strings.NewReader(`{"skills":[],"mcp":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	putResp, err := http.DefaultClient.Do(resp)
	if err != nil {
		t.Fatal(err)
	}
	putResp.Body.Close()
	if putResp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("PUT without hook status = %d, want 503", putResp.StatusCode)
	}

	// Malformed JSON: 400.
	var appliedSkills, appliedMCP []string
	_, ts2 := newClaudeEnvServer(t, func(skills, mcp []string) error {
		appliedSkills, appliedMCP = skills, mcp
		return nil
	})
	req, _ := http.NewRequest(http.MethodPut, ts2.URL+"/api/claudeenv", strings.NewReader(`{bad`))
	badResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	badResp.Body.Close()
	if badResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("PUT bad JSON status = %d, want 400", badResp.StatusCode)
	}

	// A failing apply surfaces as 500.
	_, ts3 := newClaudeEnvServer(t, func(skills, mcp []string) error {
		return errors.New("apply boom")
	})
	req, _ = http.NewRequest(http.MethodPut, ts3.URL+"/api/claudeenv", strings.NewReader(`{"skills":["a"],"mcp":[]}`))
	failResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	failResp.Body.Close()
	if failResp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("PUT apply error status = %d, want 500", failResp.StatusCode)
	}

	// Happy path echoes the selection and notes the MCP restart.
	req, _ = http.NewRequest(http.MethodPut, ts2.URL+"/api/claudeenv",
		strings.NewReader(`{"skills":["code-review"],"mcp":["fetch"]}`))
	okResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer okResp.Body.Close()
	var out struct {
		OK         bool     `json:"ok"`
		Skills     []string `json:"skills"`
		MCP        []string `json:"mcp"`
		MCPRestart bool     `json:"mcp_restart"`
	}
	if err := json.NewDecoder(okResp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if !out.OK || len(out.Skills) != 1 || out.Skills[0] != "code-review" || len(out.MCP) != 1 || !out.MCPRestart {
		t.Fatalf("PUT response = %+v", out)
	}
	if len(appliedSkills) != 1 || appliedSkills[0] != "code-review" || len(appliedMCP) != 1 || appliedMCP[0] != "fetch" {
		t.Fatalf("apply hook saw skills=%v mcp=%v", appliedSkills, appliedMCP)
	}
}

// TestClaudeEnvGetDetectionFailures redirects HOME to a tree where the
// detection sources are unreadable types: detection degrades to empty lists
// (never null, never a 500).
func TestClaudeEnvGetDetectionFailures(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home) // no t.Parallel: mutates HOME for os.UserHomeDir
	// ~/.claude/skills as a regular file: ReadDir fails with ENOTDIR.
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude", "skills"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// ~/.claude.json as a directory: ReadFile fails with EISDIR.
	if err := os.MkdirAll(filepath.Join(home, ".claude.json"), 0o755); err != nil {
		t.Fatal(err)
	}

	s := New(Config{Static: fstest.MapFS{}, Logger: zerolog.Nop()})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/claudeenv")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d, want 200 despite unreadable sources", resp.StatusCode)
	}
	var out struct {
		Skills   []json.RawMessage `json:"skills"`
		MCP      []json.RawMessage `json:"mcp"`
		Selected struct {
			Skills []string `json:"skills"`
			MCP    []string `json:"mcp"`
		} `json:"selected"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Skills == nil || out.MCP == nil || out.Selected.Skills == nil || out.Selected.MCP == nil {
		t.Fatalf("failed detection must still yield non-null arrays: %+v", out)
	}
}
