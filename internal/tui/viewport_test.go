package tui

import "testing"

func TestViewportAutoScrollToBottom(t *testing.T) {
	v := NewViewport()
	v.SetHeight(3)
	v.SetLines([]string{"a", "b", "c"})
	if got := v.Visible(); len(got) != 3 || got[0] != "a" || got[2] != "c" {
		t.Fatalf("visible = %v", got)
	}
	// Append a line while at bottom: should auto-scroll so the newest is shown.
	v.SetLines([]string{"a", "b", "c", "d"})
	if !v.AtBottom() {
		t.Fatal("should be at bottom after append")
	}
	if got := v.Visible(); len(got) != 3 || got[2] != "d" {
		t.Fatalf("visible after append = %v", got)
	}
}

func TestViewportScrollClamping(t *testing.T) {
	v := NewViewport()
	v.SetHeight(3)
	v.SetLines([]string{"a", "b", "c", "d", "e"})
	v.ScrollToBottom()
	if got := v.Visible(); got[2] != "e" {
		t.Fatalf("bottom visible = %v", got)
	}
	v.ScrollUp(1)
	if got := v.Visible(); got[0] != "b" || got[2] != "d" {
		t.Fatalf("scroll up visible = %v", got)
	}
	v.ScrollUp(100)
	if got := v.Visible(); got[0] != "a" {
		t.Fatalf("clamped top visible = %v", got)
	}
	v.ScrollDown(100)
	if !v.AtBottom() {
		t.Fatal("clamped bottom")
	}
}

func TestViewportPageUpDown(t *testing.T) {
	v := NewViewport()
	v.SetHeight(3)
	v.SetLines([]string{"a", "b", "c", "d", "e", "f", "g"})
	v.ScrollToBottom()
	v.PageUp()
	if got := v.Visible(); got[2] != "d" {
		t.Fatalf("page up visible = %v", got)
	}
	v.PageDown()
	if !v.AtBottom() {
		t.Fatal("page down should reach bottom")
	}
}
