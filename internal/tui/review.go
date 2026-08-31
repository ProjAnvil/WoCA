package tui

import "strings"

// reviewView renders the full-screen transcript review overlay: a scrolling
// window over the structured blocks with the selection kept visible.
func (m *Model) reviewView() string {
	lines, starts := m.reviewLinesWithStarts()
	height := m.height - 4 // header, footer, and padding
	if height < 1 {
		height = 1
	}
	// Keep the selected block's first line inside the window.
	selLine := 0
	if m.reviewSel < len(starts) {
		selLine = starts[m.reviewSel]
	}
	if selLine < m.reviewScroll {
		m.reviewScroll = selLine
	}
	if selLine >= m.reviewScroll+height {
		m.reviewScroll = selLine - height + 1
	}
	maxScroll := len(lines) - height
	if maxScroll < 0 {
		maxScroll = 0
	}
	if m.reviewScroll > maxScroll {
		m.reviewScroll = maxScroll
	}
	end := m.reviewScroll + height
	if end > len(lines) {
		end = len(lines)
	}

	var b strings.Builder
	b.WriteString(reviewHeader(m.reviewSel, len(m.blocks)))
	b.WriteString("\n\n")
	for _, l := range lines[m.reviewScroll:end] {
		b.WriteString(l)
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(dimStyle.Render(reviewFooter))
	return boxStyle.Render(b.String())
}
