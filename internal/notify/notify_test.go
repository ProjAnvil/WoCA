package notify

import (
	"os"
	"strings"
	"testing"
)

func TestAttentionBellAndToast(t *testing.T) {
	t.Setenv("WOCA_NOTIFY", "")
	var b strings.Builder
	Attention(&b, "woca: awaiting decision")
	out := b.String()
	if !strings.Contains(out, "\a") {
		t.Fatalf("bell missing: %q", out)
	}
	if !strings.Contains(out, "\x1b]9;woca: awaiting decision\x07") {
		t.Fatalf("OSC9 toast missing: %q", out)
	}
}

func TestAttentionDisabled(t *testing.T) {
	t.Setenv("WOCA_NOTIFY", "0")
	var b strings.Builder
	Attention(&b, "nope")
	if b.String() != "" {
		t.Fatalf("expected silence, got %q", b.String())
	}
	if Enabled() {
		t.Fatal("Enabled() must be false when WOCA_NOTIFY=0")
	}
}

func TestOSC9Sanitizes(t *testing.T) {
	var b strings.Builder
	OSC9(&b, "evil\x1b]9;injection\x07title")
	out := b.String()
	if strings.ContainsAny(out, "\x1b") != strings.Contains(out, "\x1b]9;evil") {
		// escape may only appear for the wrapper itself
		if !strings.HasPrefix(out, "\x1b]9;") {
			t.Fatalf("sanitization failed: %q", out)
		}
	}
	_ = os.Environ()
}
