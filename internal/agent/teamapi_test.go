package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"
	"github.com/projanvil/langchain-golang/core/runnables"

	"github.com/projanvil/woca/internal/agents"
)

func TestEnsureTeamIsIdempotent(t *testing.T) {
	lead, _ := newTeamLead(t, language.NewFakeChatModel(language.WithResponses(messages.AI("idle"))))
	first := lead.ensureTeam()
	second := lead.ensureTeam()
	if first != second {
		t.Fatal("ensureTeam must reuse the existing team")
	}
	if lead.currentTeam() != first {
		t.Fatal("currentTeam must snapshot the same team")
	}
	if lead.currentTaskList() != first.list {
		t.Fatal("task list must be re-scoped to the team")
	}
}

func TestTeamSpawnValidation(t *testing.T) {
	lead, _ := newTeamLead(t, language.NewFakeChatModel(language.WithResponses(messages.AI("idle"))))
	team := lead.ensureTeam()
	if err := team.Spawn(context.Background(), "", "p", nil); err == nil {
		t.Fatal("empty name must fail")
	}
	if err := team.Spawn(context.Background(), LeadName, "p", nil); err == nil {
		t.Fatal("lead name must be reserved")
	}
	// A failing subagent build surfaces the factory error.
	lead.subagentModel = nil // New() rejects a nil model
	if err := team.Spawn(context.Background(), "broken", "p", nil); err == nil {
		t.Fatal("factory failure must propagate")
	}
}

func TestTeamToolsViaExecute(t *testing.T) {
	lead, _ := newTeamLead(t, language.NewFakeChatModel(language.WithResponses(messages.AI("ack"))))

	// No team yet: the status/stop tools report it.
	if res := lead.execute(context.Background(), "TeamStatus", map[string]any{}); !strings.Contains(res.Content, "No team spawned yet") {
		t.Fatalf("TeamStatus = %q", res.Content)
	}
	if res := lead.execute(context.Background(), "TeamStop", map[string]any{"name": "x"}); !strings.Contains(res.Content, "No team spawned yet") {
		t.Fatalf("TeamStop = %q", res.Content)
	}
	// Spawn validation through the tool.
	if res := lead.execute(context.Background(), "TeamSpawn", map[string]any{"name": "", "prompt": "p"}); !strings.Contains(res.Content, "requires name and prompt") {
		t.Fatalf("TeamSpawn = %q", res.Content)
	}
	if res := lead.execute(context.Background(), "TeamSpawn", map[string]any{"name": "n", "prompt": "p", "subagent_type": "nope"}); !strings.Contains(res.Content, "unknown subagent_type") {
		t.Fatalf("TeamSpawn type = %q", res.Content)
	}

	// A real spawn registers the member and re-scopes the task tools.
	if res := lead.execute(context.Background(), "TeamSpawn", map[string]any{"name": "bob", "prompt": "help out"}); !strings.Contains(res.Content, "Spawned teammate bob") {
		t.Fatalf("TeamSpawn = %q", res.Content)
	}
	team := lead.currentTeam()
	waitFor(t, 10*time.Second, func() bool {
		team.mu.Lock()
		defer team.mu.Unlock()
		m := team.members["bob"]
		return m != nil && m.status == teammateIdle
	}, "bob idle after spawn")

	if res := lead.execute(context.Background(), "TeamStatus", map[string]any{}); !strings.Contains(res.Content, "bob") {
		t.Fatalf("TeamStatus = %q", res.Content)
	}
	if res := lead.execute(context.Background(), "TeamStop", map[string]any{"name": "ghost"}); !strings.Contains(res.Content, "no teammate") {
		t.Fatalf("TeamStop unknown = %q", res.Content)
	}
	if res := lead.execute(context.Background(), "TeamStop", map[string]any{"name": "bob", "reason": "done"}); !strings.Contains(res.Content, "Stopping teammate bob") {
		t.Fatalf("TeamStop = %q", res.Content)
	}
	waitFor(t, 10*time.Second, func() bool {
		team.mu.Lock()
		defer team.mu.Unlock()
		return team.members["bob"].status == teammateStopped
	}, "bob stopped")
}

func TestTeamSpawnWithDefinition(t *testing.T) {
	mate := language.NewFakeChatModel(language.WithResponses(messages.AI("working")))
	def := agents.Definition{Name: "coder", Prompt: "code things"}
	lead, _ := newTeamLead(t, mate)
	team := lead.ensureTeam()
	if err := team.Spawn(context.Background(), "carol", "build it", &def); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, func() bool {
		team.mu.Lock()
		defer team.mu.Unlock()
		return team.members["carol"] != nil
	}, "carol registered")
	team.Stop("carol", "")
}

func TestTeammateProcessesInboxMessage(t *testing.T) {
	// The teammate idles (no tasks), then a message wakes it: its next turn
	// consumes the <team-message> block.
	lead, _ := newTeamLead(t, language.NewFakeChatModel(language.WithResponses(messages.AI("message handled"))))
	team := lead.ensureTeam()
	if err := team.Spawn(context.Background(), "dana", "stand by", nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, func() bool {
		team.mu.Lock()
		defer team.mu.Unlock()
		m := team.members["dana"]
		return m != nil && m.status == teammateIdle
	}, "dana idle")

	team.Send(LeadName, "dana", "please check the build")
	waitFor(t, 10*time.Second, func() bool {
		team.mu.Lock()
		defer team.mu.Unlock()
		return team.members["dana"].turns >= 1
	}, "dana processed the message")

	// Stop with an empty reason skips the shutdown message but still cancels.
	if err := team.Stop("dana", ""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, func() bool {
		team.mu.Lock()
		defer team.mu.Unlock()
		return team.members["dana"].status == teammateStopped
	}, "dana stopped")
}

// scriptThenErrorModel answers once then fails every later call, so a
// teammate's first turn succeeds and its second turn errors.
type scriptThenErrorModel struct {
	*language.FakeChatModel
	answer messages.Message
	used   bool
}

func (m *scriptThenErrorModel) Invoke(ctx context.Context, input []messages.Message, opts ...runnables.Option) (messages.Message, error) {
	if m.used {
		return messages.Message{}, errScriptExhausted
	}
	m.used = true
	return m.answer, nil
}

func (m *scriptThenErrorModel) Stream(ctx context.Context, input []messages.Message, opts ...runnables.Option) (runnables.Stream[messages.Message], error) {
	return nil, errScriptExhausted
}

var errScriptExhausted = context.DeadlineExceeded

func TestTeammateTurnErrorRecordsNote(t *testing.T) {
	lead, _ := newTeamLead(t, language.NewFakeChatModel(language.WithResponses(messages.AI("dummy"))))
	lead.subagentModel = &scriptThenErrorModel{
		FakeChatModel: language.NewFakeChatModel(),
		answer:        messages.AI("first turn ok"),
	}
	team := lead.ensureTeam()
	// Give the teammate a claimable task so its turn runs immediately.
	team.list.CreateDeps("job", "do it", "", nil)
	if err := team.Spawn(context.Background(), "erin", "work", nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, func() bool {
		team.mu.Lock()
		defer team.mu.Unlock()
		m := team.members["erin"]
		return m != nil && strings.HasPrefix(m.note, "turn error")
	}, "erin turn error note")
	// Stopping during the backoff exits promptly.
	if err := team.Stop("erin", "wrap"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, func() bool {
		team.mu.Lock()
		defer team.mu.Unlock()
		return team.members["erin"].status == teammateStopped
	}, "erin stopped")
}

func TestClaimNextSkipsBlockedAndOwned(t *testing.T) {
	lead, _ := newTeamLead(t, language.NewFakeChatModel(language.WithResponses(messages.AI("idle"))))
	team := lead.ensureTeam()
	a := team.list.CreateDeps("a", "", "", nil)
	b := team.list.CreateDeps("b", "", "", []string{a.ID})
	// b is blocked while a is pending.
	if _, ok := team.claimNext("w1"); !ok {
		t.Fatal("a should be claimable")
	}
	// b stays blocked until a completes.
	if _, ok := team.claimNext("w2"); ok {
		t.Fatal("b must stay blocked")
	}
	team.list.Update(a.ID, TaskRecordCompleted, "", "")
	if rec, ok := team.claimNext("w2"); !ok || rec.ID != b.ID {
		t.Fatalf("claim after dep completed = %+v, %v", rec, ok)
	}
}

func TestReleaseOwnedReturnsClaimedTasks(t *testing.T) {
	lead, _ := newTeamLead(t, language.NewFakeChatModel(language.WithResponses(messages.AI("idle"))))
	team := lead.ensureTeam()
	rec := team.list.CreateDeps("owned", "", "", nil)
	if _, ok := team.list.Claim(rec.ID, "frank"); !ok {
		t.Fatal("claim failed")
	}
	team.releaseOwned("frank")
	if got, _ := team.list.Get(rec.ID); got.Status != TaskRecordPending || got.Owner != "" {
		t.Fatalf("after release = %+v", got)
	}
	// Another owner's tasks are untouched.
	if _, ok := team.list.Claim(rec.ID, "grace"); !ok {
		t.Fatal("re-claim failed")
	}
	team.releaseOwned("someone-else")
	if got, _ := team.list.Get(rec.ID); got.Owner != "grace" {
		t.Fatalf("foreign release touched the task: %+v", got)
	}
}

func TestTeamStatusInboxTruncates(t *testing.T) {
	lead, _ := newTeamLead(t, language.NewFakeChatModel(language.WithResponses(messages.AI("idle"))))
	team := lead.ensureTeam()
	long := strings.Repeat("x", 400)
	team.bus.Send("alice", LeadName, long)
	status := team.Status()
	if !strings.Contains(status, "from alice") {
		t.Fatalf("status missing inbox:\n%s", status)
	}
	if strings.Contains(status, long) {
		t.Fatal("inbox line not truncated to 200 chars")
	}
	// Reading acknowledged (drained): a second render has no inbox.
	if again := team.Status(); strings.Contains(again, "from alice") {
		t.Fatal("inbox not drained by reading")
	}
}

func TestTeamCloseCancelsMembers(t *testing.T) {
	lead, _ := newTeamLead(t, language.NewFakeChatModel(language.WithResponses(messages.AI("idle"))))
	team := lead.ensureTeam()
	if err := team.Spawn(context.Background(), "henry", "wait", nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, func() bool {
		team.mu.Lock()
		defer team.mu.Unlock()
		return team.members["henry"] != nil
	}, "henry registered")
	team.Close()
	waitFor(t, 10*time.Second, func() bool {
		team.mu.Lock()
		defer team.mu.Unlock()
		return team.members["henry"].status == teammateStopped
	}, "henry stopped by Close")
}

func TestTeamPromptRenderers(t *testing.T) {
	spawn := teamSpawnPrompt("alice", "brief text")
	for _, want := range []string{"alice", "lead", "brief text", "SendMessage"} {
		if !strings.Contains(spawn, want) {
			t.Errorf("teamSpawnPrompt missing %q", want)
		}
	}
	task := teamTaskPrompt(TaskRecord{ID: "task-3", Subject: "subject", Description: "details"})
	for _, want := range []string{"task-3", "subject", "details", "completed"} {
		if !strings.Contains(task, want) {
			t.Errorf("teamTaskPrompt missing %q", want)
		}
	}
	bare := teamTaskPrompt(TaskRecord{ID: "task-4", Subject: "s"})
	if strings.Contains(bare, "Details") {
		t.Error("empty description must be omitted")
	}
}
