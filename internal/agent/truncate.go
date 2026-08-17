package agent

// truncate shortens s to at most n runes, appending an ellipsis when
// anything was cut. It is rune-safe so multi-byte text (CJK) is never split
// mid-character.
func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	runes := 0
	for i := range s {
		if runes == n {
			return s[:i] + "…"
		}
		runes++
	}
	return s
}
