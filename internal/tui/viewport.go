package tui

// Viewport is a scrollable window over a list of transcript lines.
type Viewport struct {
	lines  []string
	offset int
	height int
}

// NewViewport creates an empty viewport with height 1.
func NewViewport() *Viewport {
	return &Viewport{height: 1}
}

// SetLines replaces the content and auto-scrolls to the bottom when the
// viewport was already at the bottom (so appended output stays visible).
func (v *Viewport) SetLines(lines []string) {
	atBottom := v.AtBottom()
	v.lines = lines
	if v.offset > v.maxOffset() {
		v.offset = v.maxOffset()
	}
	if atBottom {
		v.offset = v.maxOffset()
	}
}

// SetHeight sets the visible height (clamped to >= 1) and clamps offset.
func (v *Viewport) SetHeight(h int) {
	if h < 1 {
		h = 1
	}
	v.height = h
	if v.offset > v.maxOffset() {
		v.offset = v.maxOffset()
	}
}

// ScrollUp / ScrollDown move by n lines, clamped.
func (v *Viewport) ScrollUp(n int) { v.offset = clamp(v.offset-n, 0, v.maxOffset()) }
func (v *Viewport) ScrollDown(n int) {
	v.offset = clamp(v.offset+n, 0, v.maxOffset())
}

// PageUp / PageDown move by one page.
func (v *Viewport) PageUp()   { v.ScrollUp(v.height) }
func (v *Viewport) PageDown() { v.ScrollDown(v.height) }

// ScrollToBottom jumps to the newest lines.
func (v *Viewport) ScrollToBottom() { v.offset = v.maxOffset() }

// Visible returns the lines currently in view.
func (v *Viewport) Visible() []string {
	if len(v.lines) == 0 {
		return nil
	}
	end := v.offset + v.height
	if end > len(v.lines) {
		end = len(v.lines)
	}
	return v.lines[v.offset:end]
}

// AtBottom reports whether the viewport is scrolled to the bottom.
func (v *Viewport) AtBottom() bool {
	return v.offset >= v.maxOffset()
}

func (v *Viewport) maxOffset() int {
	if len(v.lines) <= v.height {
		return 0
	}
	return len(v.lines) - v.height
}

func clamp(n, lo, hi int) int {
	if n < lo {
		return lo
	}
	if n > hi {
		return hi
	}
	return n
}
