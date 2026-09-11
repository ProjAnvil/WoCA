package tui

import (
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/projanvil/langchain-golang/core/messages"
	lggraph "github.com/projanvil/langchain-golang/langgraph/graph"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/console"
	"github.com/projanvil/woca/internal/session"
)

// driveRun submits a prompt and pumps the run channel to completion.
func driveRun(t *testing.T, m *Model, text string) *Model {
	t.Helper()
	m.input = text
	mm, _ := m.Update(key("enter"))
	m = mm.(*Model)
	return drainUntilDone(t, m, m.ch)
}

// TestRunRecordsStructuredBlocks drives a full run with a tool call and
// checks the structured transcript: prompt, tool block with full args and
// untruncated result, and the answer.
func TestRunRecordsStructuredBlocks(t *testing.T) {
	m := newConsoleModel(t,
		messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
			{ID: "r1", Name: "Bash", Args: map[string]any{"command": "echo hi"}},
		}},
		messages.AI("done"),
	)
	m = driveRun(t, m, "run it")

	var user, tool, answer bool
	for i := range m.blocks {
		b := &m.blocks[i]
		switch b.kind {
		case blockUser:
			if b.text == "run it" {
				user = true
			}
		case blockTool:
			if b.tool == "Bash" && b.args["command"] == "echo hi" && b.hasRes {
				tool = true
			}
		case blockAssistant:
			if strings.Contains(b.text, "done") {
				answer = true
			}
		}
	}
	if !user || !tool || !answer {
		t.Fatalf("blocks incomplete: user=%v tool=%v answer=%v — %+v", user, tool, answer, m.blocks)
	}
}

// TestReasoningStreamsDimmedAndRecordsBlock feeds a ReasoningDelta through
// the renderer and checks both the live dim render and the reasoning block.
func TestReasoningStreamsDimmedAndRecordsBlock(t *testing.T) {
	m := New(nil, nil, config.Config{})
	ch := make(chan tea.Msg, 4)
	r := m.renderer(ch)
	r(reasoningChunk("step one "))
	r(reasoningChunk("step two"))

	mm, _ := m.Update(<-ch)
	m = mm.(*Model)
	mm, _ = m.Update(<-ch)
	m = mm.(*Model)
	m.flush()

	if len(m.blocks) != 1 || m.blocks[0].kind != blockReasoning || m.blocks[0].text != "step one step two" {
		t.Fatalf("reasoning block = %+v", m.blocks)
	}
	if !strings.Contains(strings.Join(m.lines, "\n"), "step one step two") {
		t.Fatalf("live transcript missing the reasoning text:\n%s", strings.Join(m.lines, "\n"))
	}
}

// TestReviewOverlayLifecycle opens the review overlay, walks blocks, folds
// and unfolds a tool block, jumps between prompts, and closes with esc.
func TestReviewOverlayLifecycle(t *testing.T) {
	m := newConsoleModel(t, messages.AI("ok"))
	m = driveRun(t, m, "first prompt")
	m = driveRun(t, m, "second prompt")

	m = press(m, ctrlKey("ctrl+o"))
	if m.overlay != overlayReview {
		t.Fatal("ctrl+o must open the review overlay")
	}
	view := m.View()
	if !strings.Contains(view, "transcript review") || !strings.Contains(view, "first prompt") {
		t.Fatalf("review view missing blocks:\n%s", view)
	}
	// The overlay replaces the base composer.
	if strings.Contains(view, "! bash · / commands") {
		t.Fatalf("review overlay must be full-screen:\n%s", view)
	}

	// Walking with { jumps to the previous user block.
	m = press(m, key("{"))
	if m.blocks[m.reviewSel].kind != blockUser || m.blocks[m.reviewSel].text != "first prompt" {
		t.Fatalf("{{} did not jump to the previous prompt: sel=%d", m.reviewSel)
	}
	m = press(m, key("}"))
	if m.blocks[m.reviewSel].kind != blockUser || m.blocks[m.reviewSel].text != "second prompt" {
		t.Fatalf("{}} did not jump to the next prompt: sel=%d", m.reviewSel)
	}

	// esc closes back to the base view.
	m = press(m, key("esc"))
	if m.overlay != overlayNone {
		t.Fatal("esc must close the review overlay")
	}
	if !strings.Contains(m.View(), "! bash · / commands") {
		t.Fatal("base view missing after closing the overlay")
	}
}

// TestReviewToolBlockUnfolds checks the fold interaction: collapsed shows a
// truncated summary, expanded shows the full result untruncated.
func TestReviewToolBlockUnfolds(t *testing.T) {
	long := strings.Repeat("x", 500)
	m := New(nil, nil, config.Config{})
	mm, _ := m.Update(toolCallMsg{id: "b1", name: "Bash", args: map[string]any{"command": "cat big"}})
	m = mm.(*Model)
	mm, _ = m.Update(toolResultMsg{id: "b1", content: long})
	m = mm.(*Model)

	m.openReview()
	// Find the tool block.
	sel := -1
	for i := range m.blocks {
		if m.blocks[i].kind == blockTool {
			sel = i
			break
		}
	}
	if sel < 0 {
		t.Fatal("no tool block recorded")
	}
	m.reviewSel = sel
	collapsed := m.View()
	if strings.Contains(collapsed, strings.Repeat("x", 300)) {
		t.Fatalf("collapsed view must truncate the result:\n%s", collapsed)
	}
	m = press(m, key("enter")) // unfold
	expanded := m.View()
	if !strings.Contains(expanded, long) {
		t.Fatalf("expanded view must show the full result:\n%s", expanded)
	}
}

// TestOverlayKeysWhileRunningKeepsBaseUpdating checks that an open overlay
// does not stall the base transcript: stream messages keep appending.
func TestOverlayKeysWhileRunningKeepsBaseUpdating(t *testing.T) {
	m := New(nil, nil, config.Config{})
	m.openReview()
	mm, _ := m.Update(streamMsg("still streaming"))
	m = mm.(*Model)
	m.flush()
	if !strings.Contains(strings.Join(m.lines, "\n"), "still streaming") {
		t.Fatal("an open overlay must not block base updates")
	}
}

// TestPaletteFiltersAndRuns types a fuzzy filter, moves the selection, and
// executes the highlighted command through the console layer.
func TestPaletteFiltersAndRuns(t *testing.T) {
	m := newConsoleModel(t, messages.AI("ok"))
	m = press(m, ctrlKey("ctrl+k"))
	if m.overlay != overlayPalette {
		t.Fatal("ctrl+k must open the palette")
	}
	for _, r := range "/th" {
		m = press(m, key(string(r)))
	}
	filtered := m.paletteFiltered()
	if len(filtered) == 0 || filtered[0].name != "/theme" {
		t.Fatalf("filter '/th' = %+v", filtered)
	}
	for _, c := range filtered {
		if c.group != groupCommands {
			t.Fatalf("slash filter must only match commands: %+v", filtered)
		}
	}
	m = press(m, key("enter"))
	if m.overlay != overlayNone {
		t.Fatal("enter must close the palette")
	}
	m.flush()
	if !strings.Contains(strings.Join(m.lines, "\n"), "theme") {
		t.Fatalf("palette command did not run:\n%s", strings.Join(m.lines, "\n"))
	}
}

// TestSessionsPickerListsAndLoads saves two snapshots, opens the picker,
// filters, and resumes one through the console.
func TestSessionsPickerListsAndLoads(t *testing.T) {
	dir := t.TempDir()
	// A console with an isolated session directory.
	a := newFakeAgent(t, messages.AI("ok"))
	cfg := config.Config{}
	c := console.New(console.Deps{Agent: a, Cfg: &cfg, SessionDir: dir})
	m := New(a, nil, cfg)
	m.SetConsole(c)

	// Two named snapshots with different content.
	for _, name := range []string{"alpha", "beta"} {
		path := filepath.Join(dir, name+".json")
		if err := session.SaveState(path, session.State{Messages: []messages.Message{
			messages.Human("prompt for " + name),
			messages.AI("answer for " + name),
		}}); err != nil {
			t.Fatal(err)
		}
	}

	m = press(m, ctrlKey("ctrl+r"))
	if m.overlay != overlaySessions {
		t.Fatal("ctrl+r must open the session picker")
	}
	if len(m.sessionsEntries) != 2 {
		t.Fatalf("picker entries = %+v", m.sessionsEntries)
	}
	for _, r := range "alp" {
		m = press(m, key(string(r)))
	}
	m = press(m, key("enter"))
	if m.overlay != overlayNone {
		t.Fatal("enter must close the picker")
	}
	if len(m.history) != 2 || m.history[0].Content != "prompt for alpha" {
		t.Fatalf("resume did not load alpha: %+v", m.history)
	}
	m.flush()
	if !strings.Contains(strings.Join(m.lines, "\n"), `resumed "alpha"`) {
		t.Fatalf("transcript missing the resume notice:\n%s", strings.Join(m.lines, "\n"))
	}
}

// reasoningChunk builds a custom-stream chunk carrying a reasoning delta.
func reasoningChunk(text string) lggraph.StreamChunk {
	return lggraph.StreamChunk{Mode: lggraph.StreamCustom, Payload: agent.ReasoningDelta{Text: text}}
}
