package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/tools"
)

// newTeamLead builds a lead agent whose subagents run on mateModel.
func newTeamLead(t *testing.T, mateModel language.ChatModel) (*Agent, string) {
	t.Helper()
	dir := t.TempDir()
	a, err := New(Config{
		Model:          language.NewFakeChatModel(language.WithResponses(messages.AI("lead idle"))),
		SubagentModel:  mateModel,
		Registry:       tools.NewRegistry(dir),
		SystemPrompt:   "lead",
		WorkDir:        dir,
		PermissionMode: config.ModeBypass,
	})
	if err != nil {
		t.Fatal(err)
	}
	return a, dir
}

// teammateModelScripts returns a fake model whose single turn: claims the
// task, completes it, messages the lead, then ends with text.
func teammateModelScript() language.ChatModel {
	return language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(
			messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
				{ID: "c1", Name: "TaskUpdate", Args: map[string]any{"task_id": "task-1", "status": "in_progress"}},
			}},
			messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
				{ID: "c2", Name: "TaskUpdate", Args: map[string]any{"task_id": "task-1", "status": "completed"}},
			}},
			messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
				{ID: "c3", Name: "SendMessage", Args: map[string]any{"recipient": "lead", "content": "task-1 done"}},
			}},
			messages.Message{Role: messages.RoleAI, Content: "finished task-1"},
		),
	)
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestTeamSpawnClaimsAndCompletes(t *testing.T) {
	lead, _ := newTeamLead(t, teammateModelScript())
	team := lead.ensureTeam()
	rec := team.list.CreateDeps("write the report", "details here", "", nil)
	if rec.ID != "task-1" {
		t.Fatalf("task id = %s", rec.ID)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := team.Spawn(ctx, "alice", "You write reports.", nil); err != nil {
		t.Fatal(err)
	}
	if err := team.Spawn(ctx, "alice", "duplicate", nil); err == nil {
		t.Fatal("duplicate spawn must fail")
	}

	waitFor(t, 15*time.Second, func() bool {
		r, ok := team.list.Get("task-1")
		return ok && r.Status == TaskRecordCompleted
	}, "task-1 completed by alice")
	got, _ := team.list.Get("task-1")
	if got.Owner != "alice" {
		t.Fatalf("owner = %q, want alice", got.Owner)
	}

	// The teammate reported back and went idle.
	waitFor(t, 15*time.Second, func() bool {
		team.mu.Lock()
		defer team.mu.Unlock()
		m := team.members["alice"]
		return m != nil && m.status == teammateIdle
	}, "alice idle")
	waitFor(t, 5*time.Second, func() bool {
		return len(team.bus.All()) > 0
	}, "lead inbox message")

	status := team.Status()
	for _, want := range []string{"alice", "idle", "task-1", "completed", "task-1 done"} {
		if !strings.Contains(status, want) {
			t.Fatalf("TeamStatus missing %q:\n%s", want, status)
		}
	}

	// Stop cleanly.
	if err := team.Stop("alice", "wrapping up"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool {
		team.mu.Lock()
		defer team.mu.Unlock()
		return team.members["alice"].status == teammateStopped
	}, "alice stopped")
}

func TestTeamSharedListClaimRules(t *testing.T) {
	lead, _ := newTeamLead(t, teammateModelScript())
	team := lead.ensureTeam()
	first := team.list.CreateDeps("first", "", "", nil)
	second := team.list.CreateDeps("second", "", "", []string{first.ID})
	_ = second

	// Dependency gating: second is blocked while first is pending.
	if _, ok := team.claimNext("alice"); !ok {
		t.Fatal("first should be claimable")
	}
	if rec, _ := team.list.Get(first.ID); rec.Owner != "alice" {
		t.Fatalf("first owner = %q", rec.Owner)
	}
	// claimNext skips the blocked second task.
	if _, ok := team.claimNext("bob"); ok {
		t.Fatal("second must stay blocked while first is incomplete")
	}
	// Double-claim rejected; release lets another member take over.
	if _, ok := team.list.Claim(first.ID, "bob"); ok {
		t.Fatal("claimed task cannot be re-claimed")
	}
	if !team.list.ReleaseForOwner(first.ID, "alice") {
		t.Fatal("release failed")
	}
	if _, ok := team.list.Claim(first.ID, "bob"); !ok {
		t.Fatal("released task should be claimable by bob")
	}

	// Ownership guard on TaskUpdate: bob cannot move alice's task.
	third := team.list.CreateDeps("third", "", "", nil)
	if _, ok := team.list.Claim(third.ID, "alice"); !ok {
		t.Fatal("claim third")
	}
	bob, err := lead.newSubagent(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	bob.agentName = "bob"
	bob.team = team
	bob.taskList = team.list
	if _, err := bob.taskUpdate(context.Background(), taskUpdateArgs{TaskID: third.ID, Status: "in_progress"}); err == nil {
		t.Fatal("bob must not update alice's task")
	}
	// The lead overrides.
	lead.agentName = LeadName
	if _, err := lead.taskUpdate(context.Background(), taskUpdateArgs{TaskID: third.ID, Status: "completed"}); err != nil {
		t.Fatalf("lead override failed: %v", err)
	}
}

func TestTeamSpawnCap(t *testing.T) {
	lead, _ := newTeamLead(t, nil)
	team := lead.ensureTeam()
	defer team.Close()
	for i := 0; i < maxActiveTeammates; i++ {
		if err := team.Spawn(context.Background(), fmt.Sprintf("mate-%d", i), "idle", nil); err != nil {
			t.Fatalf("spawn %d: %v", i, err)
		}
	}
	err := team.Spawn(context.Background(), "one-too-many", "idle", nil)
	if err == nil || !strings.Contains(err.Error(), "上限") {
		t.Fatalf("err = %v, want cap error with guidance", err)
	}
	if n := team.ActiveCount(); n != maxActiveTeammates {
		t.Fatalf("ActiveCount = %d, want %d", n, maxActiveTeammates)
	}
}
