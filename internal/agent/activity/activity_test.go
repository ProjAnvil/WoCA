package activity

import "testing"

// TestClampTailBytes moved from internal/agent/activity_test.go when the
// seam was extracted (spec F1) — the bounds are seam policy, so the pin
// lives with the code. Identifier names adapted to the package's own
// (TailBytes*); the cases and expectations are unchanged.
func TestClampTailBytes(t *testing.T) {
	cases := []struct{ in, want int }{
		{0, TailBytesDefault},
		{-5, TailBytesDefault},
		{100, 100},
		{TailBytesMax, TailBytesMax},
		{TailBytesMax + 1, TailBytesMax},
		{1 << 20, TailBytesMax},
	}
	for _, tc := range cases {
		if got := clampTailBytes(tc.in); got != tc.want {
			t.Errorf("clampTailBytes(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}
