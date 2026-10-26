package executor

import (
	"slices"
	"testing"
)

// TestWrapArgv pins the exact wrapper: a self-recording shell that execs the
// real argv, so a later KillArgv can target exactly that process by pid.
func TestWrapArgv(t *testing.T) {
	got := WrapArgv("abc123", []string{"sleep", "30"})
	want := []string{
		"sh", "-c",
		`echo $$ > /tmp/.woca-ses-abc123.pid; exec "$@"`,
		"sh", "sleep", "30",
	}
	if !slices.Equal(got, want) {
		t.Errorf("WrapArgv = %q,\nwant %q", got, want)
	}
}

// TestWrapArgvEmptyArgv: wrapping an empty argv is still a valid sh line
// ("exec $@" with no args is a no-op exit 0) — the driver layer rejects
// empty commands before ever wrapping.
func TestWrapArgvEmptyArgv(t *testing.T) {
	got := WrapArgv("s", nil)
	want := []string{
		"sh", "-c",
		`echo $$ > /tmp/.woca-ses-s.pid; exec "$@"`,
		"sh",
	}
	if !slices.Equal(got, want) {
		t.Errorf("WrapArgv(nil) = %q,\nwant %q", got, want)
	}
}

// TestWrapArgvRejectsUnsafeSID: the sid lands inside a sh -c script, so any
// sid carrying shell or path metacharacters must be refused (nil), never
// interpolated.
func TestWrapArgvRejectsUnsafeSID(t *testing.T) {
	for _, sid := range []string{
		"", "a;rm", "a b", "a/b", `a"b`, "a$b", "a\nb", "a|b", "..", "a`b",
	} {
		if got := WrapArgv(sid, []string{"true"}); got != nil {
			t.Errorf("WrapArgv(%q) = %q, want nil", sid, got)
		}
	}
}

// TestKillArgv pins the exact kill command for both signal-name and
// signal-number forms.
func TestKillArgv(t *testing.T) {
	tests := []struct {
		sig  string
		want string
	}{
		{"TERM", `kill -TERM $(cat /tmp/.woca-ses-abc123.pid)`},
		{"KILL", `kill -KILL $(cat /tmp/.woca-ses-abc123.pid)`},
		{"9", `kill -9 $(cat /tmp/.woca-ses-abc123.pid)`},
	}
	for _, tt := range tests {
		got := KillArgv("abc123", tt.sig)
		want := []string{"sh", "-c", tt.want}
		if !slices.Equal(got, want) {
			t.Errorf("KillArgv(sig=%q) = %q,\nwant %q", tt.sig, got, want)
		}
	}
}

// TestKillArgvRejectsUnsafe: both the sid and the signal are interpolated
// into a sh script; anything outside the safe charset is refused.
func TestKillArgvRejectsUnsafe(t *testing.T) {
	for _, sid := range []string{"", "a;b", "a b", "a/b"} {
		if got := KillArgv(sid, "TERM"); got != nil {
			t.Errorf("KillArgv(sid=%q) = %q, want nil", sid, got)
		}
	}
	for _, sig := range []string{"", "TERM; rm -rf /", "TERM && x", "KILL\n", "-9"} {
		if got := KillArgv("abc123", sig); got != nil {
			t.Errorf("KillArgv(sig=%q) = %q, want nil", sig, got)
		}
	}
}
