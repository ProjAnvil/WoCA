package sdk

import (
	"errors"
	"testing"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/agentbuild"
	"github.com/projanvil/woca/internal/tools"
)

func TestBuildOptionsTranslatesFields(t *testing.T) {
	o := Options{
		Model: "claude-sonnet-4-5", Provider: "anthropic", APIKey: "k",
		BaseURL: "https://x", Workdir: "/tmp/w", ExtraPrompt: "be brief",
		PermissionMode: "bypassPermissions", MaxTurns: 3, MaxTokens: 4096,
		ThinkingBudget: 1024, Temperature: 0.5,
		MCPServers: []string{"s=cmd"}, AllowCommands: []string{"git"},
		DenyCommands: []string{"rm -rf"}, DB: "none",
		FallbackModel: "gpt-4o", CheckpointDir: "/tmp/cp",
	}
	bo := o.buildOptions("do it")
	want := agentbuild.Options{
		Prompt: "do it", Model: "claude-sonnet-4-5", Provider: "anthropic",
		APIKey: "k", BaseURL: "https://x", MaxTokens: 4096, ThinkingBudget: 1024,
		Temperature: 0.5, Mode: "bypassPermissions", Workdir: "/tmp/w",
		ExtraPrompt: "be brief", LogLevel: "error", MCPServers: []string{"s=cmd"},
		FallbackModel: "gpt-4o", CheckpointDir: "/tmp/cp",
		AllowCommands: []string{"git"}, DenyCommands: []string{"rm -rf"},
		DB: "none", MaxTurns: 3,
	}
	if bo.Prompt != want.Prompt || bo.Model != want.Model || bo.Provider != want.Provider ||
		bo.APIKey != want.APIKey || bo.BaseURL != want.BaseURL || bo.MaxTokens != want.MaxTokens ||
		bo.Mode != want.Mode ||
		bo.DB != want.DB || bo.MaxTurns != want.MaxTurns || bo.LogLevel != want.LogLevel ||
		bo.Workdir != want.Workdir || bo.ExtraPrompt != want.ExtraPrompt ||
		bo.FallbackModel != want.FallbackModel || bo.CheckpointDir != want.CheckpointDir ||
		bo.Temperature != want.Temperature || bo.ThinkingBudget != want.ThinkingBudget ||
		len(bo.MCPServers) != 1 || bo.MCPServers[0] != "s=cmd" ||
		len(bo.AllowCommands) != 1 || bo.AllowCommands[0] != "git" ||
		len(bo.DenyCommands) != 1 || bo.DenyCommands[0] != "rm -rf" {
		t.Fatalf("translation mismatch: %+v", bo)
	}
}

func TestDefaultInterruptHandlerAutoDenies(t *testing.T) {
	h := (Options{}).interruptHandler()

	v, err := h([]agent.PauseCard{{NS: "ns-t", Value: agent.PermissionRequest{ToolName: "Bash", Reason: "r"}}})
	if err != nil || v != "n" {
		t.Fatalf("permission: got %v, %v", v, err)
	}
	v, err = h([]agent.PauseCard{{NS: "ns-t", Value: agent.AskUserRequest{}}})
	if err != nil {
		t.Fatal(err)
	}
	ans, ok := v.(agent.AskUserAnswer)
	if !ok || len(ans.Answers) != 0 {
		t.Fatalf("ask-user: got %#v", v)
	}
	v, err = h([]agent.PauseCard{{NS: "ns-t", Value: agent.PlanApprovalRequest{Plan: "p"}}})
	if err != nil || v != "n" {
		t.Fatalf("plan: got %v, %v", v, err)
	}
	_, err = h([]agent.PauseCard{{NS: "ns-t", Value: struct{}{}}})
	if err == nil {
		t.Fatal("unknown interrupt type must error")
	}
}

// TestInterruptFromApprovalEnvelope pins the "approvals" kind: every ask
// rides with its grant key and per-ask payloads, the headless default denies
// every ask, and a custom handler's ApprovalAnswer translates back.
func TestInterruptFromApprovalEnvelope(t *testing.T) {
	in := agent.ApprovalRequest{Asks: []agent.Ask{
		{CallID: "b1", ToolName: "Bash", ToolArgs: map[string]any{"command": "make"},
			Reason: "Run command: make", Origin: agent.OriginMain, SessionRuleKey: "bash:make*"},
		{CallID: "q1", ToolName: "AskUserQuestion", Origin: agent.OriginMain,
			Questions: []tools.Question{{Question: "which?"}}},
		{CallID: "e1", ToolName: "ExitPlanMode", Plan: "the plan", Origin: agent.OriginMain},
	}}
	i := interruptFrom(in)
	if i.Kind != "approvals" || i.Approvals == nil {
		t.Fatalf("interrupt = %+v, want approvals", i)
	}
	asks := i.Approvals.Asks
	if len(asks) != 3 {
		t.Fatalf("asks = %+v", asks)
	}
	if asks[0].SessionRuleKey != "bash:make*" || asks[0].Origin != agent.OriginMain {
		t.Fatalf("bash ask = %+v", asks[0])
	}
	if len(asks[1].Questions) != 1 || asks[1].Questions[0] != "which?" {
		t.Fatalf("ask_user ask = %+v", asks[1])
	}
	if asks[2].Plan != "the plan" {
		t.Fatalf("plan ask = %+v", asks[2])
	}

	// Headless default: deny every ask.
	v, err := (Options{}).interruptHandler()([]agent.PauseCard{{NS: "ns-t", ID: "id-t", Value: in}})
	if err != nil {
		t.Fatal(err)
	}
	ans, ok := v.(agent.ApprovalAnswer)
	if !ok {
		t.Fatalf("resume value = %T", v)
	}
	for _, id := range []string{"b1", "q1", "e1"} {
		if d := ans.Decisions[id]; d.Outcome != agent.OutcomeDeny {
			t.Fatalf("%s = %+v, want deny", id, d)
		}
	}

	// Custom handler answers translate per-ask decisions back.
	o := Options{OnInterrupt: func(i Interrupt) (any, error) {
		return ApprovalAnswer{Decisions: map[string]AskDecisionView{
			"b1": {Outcome: agent.OutcomeAllow, Grant: agent.GrantSession},
			"e1": {Outcome: agent.OutcomeDeny},
		}}, nil
	}}
	v, err = o.interruptHandler()([]agent.PauseCard{{NS: "ns-t", ID: "id-t", Value: in}})
	if err != nil {
		t.Fatal(err)
	}
	ans, ok = v.(agent.ApprovalAnswer)
	if !ok {
		t.Fatalf("resume value = %T", v)
	}
	if d := ans.Decisions["b1"]; d.Outcome != agent.OutcomeAllow || d.Grant != agent.GrantSession {
		t.Fatalf("b1 = %+v", d)
	}
	if d := ans.Decisions["e1"]; d.Outcome != agent.OutcomeDeny {
		t.Fatalf("e1 = %+v", d)
	}
}

func TestCustomInterruptHandlerWins(t *testing.T) {
	sentinel := errors.New("denied by user code")
	o := Options{OnInterrupt: func(i Interrupt) (any, error) {
		if i.Kind != "permission" || i.Permission.ToolName != "Bash" {
			t.Fatalf("unexpected interrupt: %+v", i)
		}
		return nil, sentinel
	}}
	_, err := o.interruptHandler()([]agent.PauseCard{{NS: "ns-t", Value: agent.PermissionRequest{ToolName: "Bash"}}})
	if !errors.Is(err, sentinel) {
		t.Fatalf("got %v", err)
	}
}

func TestCustomInterruptHandlerTranslatesAskUserAnswer(t *testing.T) {
	o := Options{OnInterrupt: func(i Interrupt) (any, error) {
		if i.Kind != "ask_user" {
			t.Fatalf("unexpected interrupt: %+v", i)
		}
		return AskUserAnswer{Answers: []string{"a", "b"}}, nil
	}}
	v, err := o.interruptHandler()([]agent.PauseCard{{NS: "ns-t", Value: agent.AskUserRequest{}}})
	if err != nil {
		t.Fatal(err)
	}
	ans, ok := v.(agent.AskUserAnswer)
	if !ok {
		t.Fatalf("expected agent.AskUserAnswer, got %#v", v)
	}
	if len(ans.Answers) != 2 || ans.Answers[0] != "a" || ans.Answers[1] != "b" {
		t.Fatalf("answers mismatch: %#v", ans)
	}
}

func TestInterruptFromSubagentPermission(t *testing.T) {
	in := agent.SubagentInterrupt{
		Subagent: "reviewer",
		Inner: agent.PermissionRequest{
			ToolName:   "Write",
			ToolArgs:   map[string]any{"path": "x"},
			Reason:     "edit file",
			IsReadOnly: false,
		},
	}
	i := interruptFrom(in)
	if i.Kind != "subagent_permission" {
		t.Fatalf("kind = %q, want subagent_permission", i.Kind)
	}
	if i.Subagent != "reviewer" {
		t.Fatalf("subagent = %q", i.Subagent)
	}
	if i.Permission == nil || i.Permission.ToolName != "Write" || i.Permission.Reason != "edit file" {
		t.Fatalf("permission = %+v", i.Permission)
	}

	// The default headless handler auto-denies subagent permission pauses.
	v, err := (Options{}).interruptHandler()([]agent.PauseCard{{NS: "ns-t", ID: "id-t", Value: in}})
	if err != nil || v != "n" {
		t.Fatalf("default handler = %v, %v", v, err)
	}
}

// TestInterruptMultiCardAggregate: a several-card pause reaches OnInterrupt
// as ONE "multi" Interrupt carrying every card with its NS key; a ListAnswer
// translates into the NS-keyed map resume, and the headless default denies
// every ask of every card.
func TestInterruptMultiCardAggregate(t *testing.T) {
	cards := []agent.PauseCard{
		{NS: "approval:se1", ID: "i1", Value: agent.ApprovalRequest{Asks: []agent.Ask{
			{CallID: "b1", ToolName: "Bash", Origin: agent.OriginMain},
		}}},
		{NS: "subagent:researcher-ad33:se2", ID: "i2", Value: agent.ApprovalRequest{Asks: []agent.Ask{
			{CallID: "w1", ToolName: "Write", Origin: "researcher"},
		}}},
	}

	// The aggregate view.
	var seen Interrupt
	o := Options{OnInterrupt: func(i Interrupt) (any, error) {
		seen = i
		return ListAnswer{
			{Key: "approval:se1", Value: ApprovalAnswer{Decisions: map[string]AskDecisionView{
				"b1": {Outcome: agent.OutcomeAllow, Grant: agent.GrantSession},
			}}},
			{Key: "subagent:researcher-ad33:se2", Value: ApprovalAnswer{Decisions: map[string]AskDecisionView{
				"w1": {Outcome: agent.OutcomeDeny},
			}}},
		}, nil
	}}
	v, err := o.interruptHandler()(cards)
	if err != nil {
		t.Fatal(err)
	}
	if seen.Kind != "multi" || len(seen.Interrupts) != 2 {
		t.Fatalf("aggregate = %+v", seen)
	}
	if seen.Interrupts[0].NS != "approval:se1" || seen.Interrupts[0].Approvals == nil {
		t.Fatalf("card 1 view = %+v", seen.Interrupts[0])
	}
	if seen.Interrupts[1].NS != "subagent:researcher-ad33:se2" {
		t.Fatalf("card 2 view = %+v", seen.Interrupts[1])
	}
	// The ListAnswer translated into the NS-keyed map.
	mp, ok := v.(map[string]any)
	if !ok || len(mp) != 2 {
		t.Fatalf("resume value = %#v, want the two-entry NS map", v)
	}
	first, ok := mp["approval:se1"].(agent.ApprovalAnswer)
	if !ok || first.Decisions["b1"].Outcome != agent.OutcomeAllow || first.Decisions["b1"].Grant != agent.GrantSession {
		t.Fatalf("card 1 answer = %#v", mp["approval:se1"])
	}
	second, ok := mp["subagent:researcher-ad33:se2"].(agent.ApprovalAnswer)
	if !ok || second.Decisions["w1"].Outcome != agent.OutcomeDeny {
		t.Fatalf("card 2 answer = %#v", mp["subagent:researcher-ad33:se2"])
	}

	// A single card keeps the plain single-kind view, now with its NS.
	v, err = o.interruptHandler()(cards[:1])
	if err != nil {
		t.Fatal(err)
	}
	_ = v

	// Headless default: deny every ask of every card.
	v, err = (Options{}).interruptHandler()(cards)
	if err != nil {
		t.Fatal(err)
	}
	mp, ok = v.(map[string]any)
	if !ok || len(mp) != 2 {
		t.Fatalf("headless resume = %#v", v)
	}
	for _, key := range []string{"approval:se1", "subagent:researcher-ad33:se2"} {
		ans, ok := mp[key].(agent.ApprovalAnswer)
		if !ok {
			t.Fatalf("headless %s = %#v, want ApprovalAnswer", key, mp[key])
		}
		for _, d := range ans.Decisions {
			if d.Outcome != agent.OutcomeDeny {
				t.Fatalf("headless decision = %+v, want deny", d)
			}
		}
	}
}
