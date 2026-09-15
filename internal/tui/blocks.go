package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// The structured transcript: a parallel block log behind the live styled
// transcript, powering the review overlay (ctrl+o). The live view keeps its
// append-only line rendering; blocks record the same events with their full
// payloads (untruncated tool args and results, reasoning text), so review
// can fold and unfold without re-running anything.

type blockKind int

const (
	blockUser blockKind = iota
	blockAssistant
	blockReasoning
	blockTool
	blockSystem
)

func (k blockKind) String() string {
	switch k {
	case blockUser:
		return "user"
	case blockAssistant:
		return "answer"
	case blockReasoning:
		return "reasoning"
	case blockTool:
		return "tool"
	default:
		return "system"
	}
}

// block is one reviewable transcript unit. Tool blocks accumulate their
// result when the matching RoleTool message arrives.
type block struct {
	kind   blockKind
	text   string // user / assistant / reasoning / system text
	toolID string
	tool   string
	args   map[string]any
	result string
	hasRes bool
}

// blocksLimit bounds the structured log (review covers recent history).
const blocksLimit = 1000

func (m *Model) addBlock(b block) {
	m.blocks = append(m.blocks, b)
	if len(m.blocks) > blocksLimit {
		m.blocks = m.blocks[len(m.blocks)-blocksLimit:]
	}
}

// appendUserBlock records a submitted prompt.
func (m *Model) appendUserBlock(text string) {
	m.addBlock(block{kind: blockUser, text: text})
}

// appendSystemBlock records command output, errors, and markers.
func (m *Model) appendSystemBlock(text string) {
	m.addBlock(block{kind: blockSystem, text: text})
}

// appendStreamText routes one stream fragment into the structured log:
// reasoning text feeds the open reasoning block (or opens one), answer text
// feeds the open assistant block (or opens one after reasoning closes it).
func (m *Model) appendStreamText(kind blockKind, text string) {
	if text == "" {
		return
	}
	if n := len(m.blocks); n > 0 && m.blocks[n-1].kind == kind {
		m.blocks[n-1].text += text
		return
	}
	m.addBlock(block{kind: kind, text: text})
}

// appendToolCall records one tool invocation.
func (m *Model) appendToolCall(id, name string, args map[string]any) {
	m.addBlock(block{kind: blockTool, toolID: id, tool: name, args: args})
}

// attachToolResult fills the result of the matching open tool block (or a
// detached system block when the id is unknown).
func (m *Model) attachToolResult(id, content string) {
	for i := len(m.blocks) - 1; i >= 0; i-- {
		if m.blocks[i].kind == blockTool && m.blocks[i].toolID == id && !m.blocks[i].hasRes {
			m.blocks[i].result = content
			m.blocks[i].hasRes = true
			return
		}
	}
}

// review rendering -----------------------------------------------------------

// reviewTitle is the overlay header.
const reviewTitle = "transcript review"

// reviewLinesWithStarts renders every block and returns the rendered lines
// plus each block's first-line index (the review overlay's scroll anchor).
func (m *Model) reviewLinesWithStarts() ([]string, []int) {
	var out []string
	var starts []int
	for i := range m.blocks {
		starts = append(starts, len(out))
		b := &m.blocks[i]
		selected := i == m.reviewSel
		marker := " "
		if selected {
			marker = "❯"
		}
		switch b.kind {
		case blockUser:
			text := b.text
			if text == "" {
				text = "(empty)"
			}
			out = append(out, marker+" "+userStyle.Render("❯ ")+firstLine(text))
			for _, l := range restLines(text) {
				out = append(out, "  "+l)
			}
		case blockAssistant:
			out = append(out, marker+" "+firstLine(b.text))
			for _, l := range restLines(b.text) {
				out = append(out, "  "+l)
			}
		case blockReasoning:
			if m.reviewFold(i) {
				n := len(restLines(b.text)) + 1
				out = append(out, marker+" "+dimStyle.Render(fmt.Sprintf("💭 thought (%d lines)", n))+"   "+dimStyle.Render("[enter] expand"))
			} else {
				out = append(out, marker+" "+dimStyle.Render("💭 thought")+"   "+dimStyle.Render("[enter] fold"))
				for l := range strings.SplitSeq(b.text, "\n") {
					out = append(out, "  "+dimStyle.Render(l))
				}
			}
		case blockTool:
			head := toolColor(b.tool).Render("⏺ " + b.tool)
			stats := reviewToolStats(b)
			if stats != "" {
				head += " " + stats
			}
			if m.reviewFold(i) {
				summary := summarize(b.result)
				if summary != "(no output)" && summary != "" {
					head += "  " + dimStyle.Render("⎿ "+summary)
				}
				out = append(out, marker+" "+head+"   "+dimStyle.Render("[enter] expand"))
			} else {
				out = append(out, marker+" "+head+"   "+dimStyle.Render("[enter] fold"))
				for _, l := range diffLines(b.args) {
					out = append(out, "  "+l)
				}
				for _, l := range reviewArgsLines(b.args) {
					out = append(out, "  "+dimStyle.Render(l))
				}
				if b.hasRes {
					out = append(out, "  "+dimStyle.Render("⎿"))
					for l := range strings.SplitSeq(strings.TrimRight(b.result, "\n"), "\n") {
						out = append(out, "  "+l)
					}
				}
			}
		case blockSystem:
			for j, l := range strings.Split(strings.TrimRight(b.text, "\n"), "\n") {
				if j == 0 {
					out = append(out, marker+" "+dimStyle.Render(l))
				} else {
					out = append(out, "  "+dimStyle.Render(l))
				}
			}
		}
	}
	if len(out) == 0 {
		out = append(out, dimStyle.Render("(nothing to review yet)"))
	}
	return out, starts
}

func firstLine(s string) string { return strings.SplitN(s, "\n", 2)[0] }
func restLines(s string) []string {
	parts := strings.SplitN(s, "\n", 2)
	if len(parts) < 2 {
		return nil
	}
	return strings.Split(strings.TrimRight(parts[1], "\n"), "\n")
}

// reviewFold reports whether block i is folded (tool and reasoning blocks
// start folded; the selected block auto-unfolds on first visit).
func (m *Model) reviewFold(i int) bool {
	if _, ok := m.reviewFolded[i]; !ok {
		m.reviewFolded[i] = true
	}
	return m.reviewFolded[i]
}

// reviewToolStats renders a compact +/- line for Edit/Write diffs.
func reviewToolStats(b *block) string {
	if b.tool != "Edit" && b.tool != "Write" && b.tool != "NotebookEdit" {
		return ""
	}
	added, removed := diffStats(b.args)
	if added == 0 && removed == 0 {
		return ""
	}
	return lipgloss.NewStyle().Foreground(lipgloss.Color("2")).Render(fmt.Sprintf("+%d", added)) +
		" " + lipgloss.NewStyle().Foreground(lipgloss.Color("1")).Render(fmt.Sprintf("−%d", removed))
}

// diffStats counts the +/− lines of an Edit/Write argument pair.
func diffStats(args map[string]any) (added, removed int) {
	if old, ok := args["old_string"].(string); ok {
		removed = len(strings.Split(strings.TrimRight(old, "\n"), "\n"))
	}
	if new, ok := args["new_string"].(string); ok && new != "" {
		added = len(strings.Split(strings.TrimRight(new, "\n"), "\n"))
	}
	if added == 0 && removed == 0 {
		if content, ok := args["content"].(string); ok && content != "" {
			added = len(strings.Split(strings.TrimRight(content, "\n"), "\n"))
		}
	}
	return
}

// reviewArgsLines renders the non-diff tool arguments compactly (the diff
// pair is rendered separately by diffLines).
func reviewArgsLines(args map[string]any) []string {
	var out []string
	for _, k := range []string{"file_path", "path", "command", "pattern", "url", "query"} {
		if v, ok := args[k]; ok {
			out = append(out, fmt.Sprintf("  %s: %v", k, v))
		}
	}
	return out
}

// reviewHeader and reviewFooter bracket the overlay.
func reviewHeader(sel, total int) string {
	return boldDimStyle.Render(fmt.Sprintf("%s — block %d/%d", reviewTitle, sel+1, total))
}

const reviewFooter = "↑↓ blocks · enter fold/unfold · { } jump prompts · q / esc close"
