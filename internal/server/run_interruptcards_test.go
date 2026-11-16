package server

// Task 8: multi-card interrupt parks. A pause with several pending cards
// parks as ONE "multi" interrupt event carrying every card (id/ns/kind/
// payload); the list-form answer body {answers: [{key, value}]} builds the
// NS-keyed map resume — partial lists resume a subset, the rest re-pause.

import (
	"context"
	"testing"
	"time"

	"github.com/projanvil/woca/internal/agent"
)

// parkCardsForTest parks the given cards on a fresh run and returns the run,
// its cancel func and the result channel.
func parkCardsForTest(t *testing.T, cards []agent.PauseCard) (*Run, context.CancelFunc, chan any) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	r := newRun("mr", nil, ctx, cancel)
	got := make(chan any, 1)
	go func() {
		v, err := r.handleInterrupt(cards)
		if err != nil {
			got <- err
		} else {
			got <- v
		}
	}()
	waitPending(t, r)
	return r, cancel, got
}

func multiCardPause() []agent.PauseCard {
	return []agent.PauseCard{
		{NS: "approval:se1", ID: "ia1", Value: agent.ApprovalRequest{Asks: []agent.Ask{
			{CallID: "b1", ToolName: "Bash", ToolArgs: map[string]any{"command": "make"}, Reason: "Run command: make", Origin: agent.OriginMain},
		}}},
		{NS: "subagent:researcher-ad33:se2", ID: "ia2", Value: agent.ApprovalRequest{Asks: []agent.Ask{
			{CallID: "w1", ToolName: "Write", ToolArgs: map[string]any{"file_path": "a.txt"}, Reason: "Write file a.txt", Origin: "researcher"},
		}}},
	}
}

func TestMultiCardParkEmitsCardList(t *testing.T) {
	r, cancel, got := parkCardsForTest(t, multiCardPause())
	defer cancel()

	r.mu.Lock()
	cards := r.pending.cards
	r.mu.Unlock()
	if len(cards) != 2 {
		t.Fatalf("parked %d cards, want 2", len(cards))
	}
	if cards[0].ns != "approval:se1" || cards[0].kind != "permission" {
		t.Fatalf("card[0] = %+v", cards[0])
	}
	if cards[1].ns != "subagent:researcher-ad33:se2" || cards[1].kind != "permission" {
		t.Fatalf("card[1] = %+v", cards[1])
	}

	// The interrupt event carries ALL pending cards under kind "multi".
	var ev *Event
	r.mu.Lock()
	for i := range r.history {
		if r.history[i].Type == EventInterrupt {
			e := r.history[i]
			ev = &e
		}
	}
	r.mu.Unlock()
	if ev == nil {
		t.Fatal("no interrupt event in history")
	}
	data, ok := ev.Data.(interruptPayload)
	if !ok {
		t.Fatalf("interrupt event data = %T", ev.Data)
	}
	if data.Kind != "multi" || data.Payload != nil {
		t.Fatalf("event = %s %v, want multi with no top-level payload", data.Kind, data.Payload)
	}
	if len(data.Cards) != 2 {
		t.Fatalf("event carries %d cards, want 2", len(data.Cards))
	}
	for i, want := range []string{"approval:se1", "subagent:researcher-ad33:se2"} {
		if data.Cards[i].NS != want || data.Cards[i].ID == "" || data.Cards[i].Kind != "permission" || data.Cards[i].Payload == nil {
			t.Fatalf("event card[%d] = %+v (want ns %q)", i, data.Cards[i], want)
		}
	}

	// A single card's flat payload keeps the legacy shape.
	if m, ok := data.Cards[1].Payload.(map[string]any); !ok || m["tool_name"] != "Write" {
		t.Fatalf("card[1] payload = %#v, want the legacy flat permission map", data.Cards[1].Payload)
	}

	// The list-form answer builds the NS-keyed map (both cards answered).
	if err := r.answer([]byte(`{"answers":[
		{"key":"approval:se1","value":{"approve":true}},
		{"key":"subagent:researcher-ad33:se2","value":{"approve":false}}
	]}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case v := <-got:
		m, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("resume value = %T, want NS-keyed map", v)
		}
		if len(m) != 2 {
			t.Fatalf("resume map = %#v, want both cards", m)
		}
		if m["approval:se1"] != true {
			t.Fatalf("card 1 answer = %#v, want true", m["approval:se1"])
		}
		if m["subagent:researcher-ad33:se2"] != false {
			t.Fatalf("card 2 answer = %#v, want false", m["subagent:researcher-ad33:se2"])
		}
	case <-time.After(5 * time.Second):
		t.Fatal("interrupt never resumed")
	}
}

func TestMultiCardPartialAnswerResumesSubset(t *testing.T) {
	r, cancel, got := parkCardsForTest(t, multiCardPause())
	defer cancel()

	// Answer ONLY the first card: the resume map carries exactly that
	// entry; the unanswered card re-pauses (fork-native).
	if err := r.answer([]byte(`{"answers":[{"key":"approval:se1","value":{"approve":true}}]}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case v := <-got:
		m, ok := v.(map[string]any)
		if !ok || len(m) != 1 || m["approval:se1"] != true {
			t.Fatalf("resume value = %#v, want the single-entry map", v)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("interrupt never resumed")
	}
}

func TestMultiCardAnswerValidations(t *testing.T) {
	t.Run("unknown key rejected", func(t *testing.T) {
		r, cancel, _ := parkCardsForTest(t, multiCardPause())
		defer cancel()
		if err := r.answer([]byte(`{"answers":[{"key":"nope","value":{"approve":true}}]}`)); err == nil {
			t.Fatal("unknown key must error")
		}
		r.mu.Lock()
		still := r.pending != nil
		r.mu.Unlock()
		if !still {
			t.Fatal("failed answer must leave the pause parked")
		}
	})

	t.Run("legacy body cannot answer a multi-card pause", func(t *testing.T) {
		r, cancel, _ := parkCardsForTest(t, multiCardPause())
		defer cancel()
		if err := r.answer([]byte(`{"approve":true}`)); err == nil {
			t.Fatal("flat body must error with several cards parked")
		}
	})

	t.Run("approvals card decodes per-ask decisions", func(t *testing.T) {
		cards := []agent.PauseCard{
			{NS: "ns-x", ID: "ix", Value: agent.ApprovalRequest{Asks: []agent.Ask{
				{CallID: "b1", ToolName: "Bash", Origin: agent.OriginMain},
				{CallID: "b2", ToolName: "Bash", Origin: agent.OriginMain},
			}}},
		}
		r, cancel, got := parkCardsForTest(t, cards)
		defer cancel()
		if err := r.answer([]byte(`{"answers":[{"key":"ns-x","value":{
			"decisions":{"b1":{"outcome":"allow","grant":"session"},"b2":{"outcome":"deny"}}
		}}]}`)); err != nil {
			t.Fatal(err)
		}
		select {
		case v := <-got:
			// Single pending card: the keyed entry resolves to a scalar resume.
			ans, ok := v.(agent.ApprovalAnswer)
			if !ok {
				t.Fatalf("resume value = %T, want ApprovalAnswer", v)
			}
			if d := ans.Decisions["b1"]; d.Outcome != agent.OutcomeAllow || d.Grant != agent.GrantSession {
				t.Fatalf("b1 = %+v", d)
			}
			if d := ans.Decisions["b2"]; d.Outcome != agent.OutcomeDeny {
				t.Fatalf("b2 = %+v", d)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("interrupt never resumed")
		}
	})

	t.Run("asks-bearing legacy card accepts the decision form (session grants)", func(t *testing.T) {
		// A single-ask envelope parks under the legacy kind with `asks`
		// attached; its decision-form answer must carry the session grant
		// (Task 11's three-button card), while the flat {approve} body
		// keeps the bool resume.
		cards := []agent.PauseCard{
			{NS: "approval:se9", ID: "i9", Value: agent.ApprovalRequest{Asks: []agent.Ask{
				{CallID: "b9", ToolName: "Bash", ToolArgs: map[string]any{"command": "git push"}, Origin: agent.OriginMain, SessionRuleKey: "bash:git push*"},
			}}},
		}
		r, cancel, got := parkCardsForTest(t, cards)
		defer cancel()
		if err := r.answer([]byte(`{"answers":[{"key":"approval:se9","value":{
			"decisions":{"b9":{"outcome":"allow","grant":"session"}}
		}}]}`)); err != nil {
			t.Fatal(err)
		}
		select {
		case v := <-got:
			ans, ok := v.(agent.ApprovalAnswer)
			if !ok {
				t.Fatalf("resume value = %T, want ApprovalAnswer", v)
			}
			if d := ans.Decisions["b9"]; d.Outcome != agent.OutcomeAllow || d.Grant != agent.GrantSession {
				t.Fatalf("b9 = %+v", d)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("interrupt never resumed")
		}

		// The flat body on the same shape still resumes the plain flag.
		r2, cancel2, got2 := parkCardsForTest(t, cards)
		defer cancel2()
		if err := r2.answer([]byte(`{"answers":[{"key":"approval:se9","value":{"approve":true}}]}`)); err != nil {
			t.Fatal(err)
		}
		select {
		case v := <-got2:
			if v != true {
				t.Fatalf("flat resume = %#v, want true", v)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("interrupt never resumed")
		}
	})
}
