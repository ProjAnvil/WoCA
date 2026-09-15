package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"testing/fstest"

	"github.com/rs/zerolog"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/skills"
	"github.com/projanvil/woca/internal/tools"
)

// TestCompletionsScopedToWorkspace: the "/" and "@" autocomplete lists must
// come from the requesting session's workspace, not only the server's
// startup directory.
func TestCompletionsScopedToWorkspace(t *testing.T) {
	base := t.TempDir()
	ws := t.TempDir()
	a, err := agent.New(agent.Config{
		Model:          language.NewFakeChatModel(language.WithResponses(messages.AI("ok"))),
		Registry:       tools.NewRegistry(base),
		SystemPrompt:   "t",
		PermissionMode: config.ModeBypass,
		WorkDir:        base,
		Skills:         []skills.Skill{{Name: "skill-base", Description: "b"}},
		SkillsFor: func(dir string) []skills.Skill {
			return []skills.Skill{{Name: "skill-of-" + dir, Description: "d"}}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := New(Config{Agent: a, Static: fstest.MapFS{}, Logger: zerolog.Nop(), WorkDir: base})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	get := func(url string) (skillsList []string, agentsList []string) {
		resp, err := http.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var payload struct {
			Skills []struct {
				Name string `json:"name"`
			} `json:"skills"`
			Agents []struct {
				Name string `json:"name"`
			} `json:"agents"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		for _, e := range payload.Skills {
			skillsList = append(skillsList, e.Name)
		}
		for _, e := range payload.Agents {
			agentsList = append(agentsList, e.Name)
		}
		return
	}

	got, _ := get(ts.URL + "/api/completions")
	if !slices.Contains(got, "skill-base") || slices.Contains(got, "skill-of-"+ws) {
		t.Fatalf("default completions = %v, want only the startup skills", got)
	}
	got, _ = get(ts.URL + "/api/completions?workspace=" + ws)
	if !slices.Contains(got, "skill-of-"+ws) {
		t.Fatalf("scoped completions = %v, want the workspace skill", got)
	}
}
