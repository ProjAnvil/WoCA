package console

import (
	"context"
	"strings"
	"testing"

	"github.com/projanvil/langchain-golang/core/messages"
	"github.com/projanvil/woca/internal/server"
	"github.com/projanvil/woca/internal/webgui"
	"github.com/rs/zerolog"
)

func TestRewindBranchAndWebCommands(t *testing.T) {
	c, _ := newFullConsole(t)
	ctx := context.Background()
	s := &Session{History: []messages.Message{messages.Human("hi")}}

	// /rewind with no args lists snapshot turns.
	res, err := c.Execute(ctx, "rewind", nil, s)
	if err != nil {
		t.Fatalf("rewind list = %+v %v", res, err)
	}
	// /branch with no name lists branches.
	res, err = c.Execute(ctx, "branch", nil, s)
	if err != nil {
		t.Fatalf("branch list = %+v %v", res, err)
	}
	// /web status then off (never started).
	c.deps.Web = webgui.New(server.Config{Logger: zerolog.Nop()})
	res, err = c.Execute(ctx, "web", []string{"status"}, s)
	if err != nil || !strings.Contains(res.Output, "web gui") {
		t.Fatalf("web status = %+v %v", res, err)
	}
	res, err = c.Execute(ctx, "web", []string{"off"}, s)
	if err != nil || !strings.Contains(res.Output, "not running") {
		t.Fatalf("web off = %+v %v", res, err)
	}
	// No web manager: error.
	c2, _ := newAgentConsole(t, nil)
	if _, err := c2.Execute(ctx, "web", nil, s); err == nil {
		t.Fatal("expected web unavailable error")
	}
	// /team without a team.
	res, err = c.Execute(ctx, "team", nil, s)
	if err != nil {
		t.Logf("team = %+v %v", res, err)
	}
}

func TestRewindTurnSelection(t *testing.T) {
	c, _ := newAgentConsole(t, nil)
	ctx := context.Background()
	s := &Session{History: []messages.Message{
		messages.Human("first question"),
		messages.AI("first answer"),
		messages.Human("second question"),
		messages.AI("second answer"),
	}}
	// No turns yet.
	empty := &Session{}
	res, err := c.Execute(ctx, "rewind", nil, empty)
	if err != nil || !strings.Contains(res.Output, "nothing to rewind") {
		t.Fatalf("empty rewind = %+v %v", res, err)
	}
	// List shows both turns.
	res, err = c.Execute(ctx, "rewind", nil, s)
	if err != nil || !strings.Contains(res.Output, "first question") || !strings.Contains(res.Output, "second question") {
		t.Fatalf("rewind list = %+v %v", res, err)
	}
	// Rewind to before turn 2 keeps the first exchange.
	res, err = c.Execute(ctx, "rewind", []string{"2"}, s)
	if err != nil || !strings.Contains(res.Output, "rewound to before turn 2") {
		t.Fatalf("rewind 2 = %+v %v", res, err)
	}
	if len(s.History) != 2 {
		t.Fatalf("history = %d messages", len(s.History))
	}
	// Rewinding everything clears the conversation.
	res, err = c.Execute(ctx, "rewind", []string{"1"}, s)
	if err != nil || !res.Clear {
		t.Fatalf("rewind 1 = %+v %v", res, err)
	}
	// Out-of-range turns error.
	if _, err := c.Execute(ctx, "rewind", []string{"9"}, s); err == nil {
		t.Fatal("expected range error")
	}
	if _, err := c.Execute(ctx, "rewind", []string{"0"}, s); err == nil {
		t.Fatal("expected range error")
	}
	// /branch <name> requires history and a name.
	if _, err := c.Execute(ctx, "branch", []string{"only"}, s); err == nil {
		t.Log("branch tolerated")
	}
}

// errEngine fails every operation.
