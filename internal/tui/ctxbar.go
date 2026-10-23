package tui

import (
	"context"
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/config"
)

// The context-usage status segment: after each completed run the footer
// shows what share of the context window the next request would fill
// (agent.ContextBreakdown is the same source behind the web UI's hover
// indicator and GET /api/sessions/{id}/context). Threshold colors are
// semantic (approaching the compact threshold), not decorative.

// ctxWarnPct / ctxDangerPct are the display thresholds.
const (
	ctxWarnPct   = 80
	ctxDangerPct = 95
	ctxBarCells  = 10
)

// refreshContext recomputes the context snapshot after a run.
func (m *Model) refreshContext() {
	if m.agent == nil {
		return
	}
	info := m.agent.ContextBreakdown(context.Background(), m.history)
	if info.ContextWindow == 0 {
		info.ContextWindow = config.ContextWindowFor(info.Model)
	}
	m.ctxInfo = &info
	if pct := ctxPercent(m.ctxInfo); pct >= ctxDangerPct {
		m.write(dimStyle.Render(fmt.Sprintf("· context %d%% — /compact to free space", pct)) + "\n")
	}
}

// ctxPercent computes the fill percentage from the context window (or the
// compact threshold when the window is unknown); 0 when neither is known.
func ctxPercent(info *agent.ContextInfo) int {
	if info == nil {
		return 0
	}
	limit := info.ContextWindow
	if limit <= 0 {
		limit = info.CompactThreshold
	}
	if limit <= 0 {
		return 0
	}
	pct := min(info.EstimatedTotal*100/limit, 999)
	return pct
}

// ctxFooter renders the status segment: a 10-cell bar, the percentage, and
// used/limit in thousands.
func ctxFooter(info *agent.ContextInfo) string {
	pct := ctxPercent(info)
	if pct == 0 && (info == nil || info.EstimatedTotal == 0) {
		return ""
	}
	limit := int64(0)
	if info != nil {
		limit = int64(info.ContextWindow)
		if limit <= 0 {
			limit = int64(info.CompactThreshold)
		}
	}
	var style lipgloss.Style
	switch {
	case pct >= ctxDangerPct:
		style = lipgloss.NewStyle().Foreground(lipgloss.Color("1")) // red
	case pct >= ctxWarnPct:
		style = lipgloss.NewStyle().Foreground(lipgloss.Color("3")) // yellow
	default:
		style = dimStyle
	}
	filled := min(pct*ctxBarCells/100, ctxBarCells)
	bar := strings.Repeat("▓", filled) + strings.Repeat("░", ctxBarCells-filled)
	s := fmt.Sprintf("ctx %s %d%%", bar, pct)
	if limit > 0 && info != nil {
		s += fmt.Sprintf(" %dk/%dk", info.EstimatedTotal/1000, limit/1000)
	}
	return style.Render(s)
}
