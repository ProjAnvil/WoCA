package agent

import (
	"context"
	"strings"
	"testing"
)

func TestDeriveSubagentThreadDeterministic(t *testing.T) {
	a := deriveSubagentThread("run-1", "call_9", "do the thing")
	b := deriveSubagentThread("run-1", "call_9", "do the thing")
	if a != b {
		t.Fatalf("not deterministic: %q vs %q", a, b)
	}
	if !strings.HasPrefix(a, "run-1/sub-call_9-") {
		t.Fatalf("shape = %q", a)
	}
	// 同 prompt 不同 call ID 不碰撞;无 call ID 时退化为 prompt 摘要。
	if c := deriveSubagentThread("run-1", "call_10", "do the thing"); c == a {
		t.Fatal("different call IDs must not collide")
	}
	if d := deriveSubagentThread("run-1", "", "do the thing"); !strings.HasPrefix(d, "run-1/sub-") {
		t.Fatalf("no-callID fallback shape = %q", d)
	}
}

func TestToolCallIDContextRoundTrip(t *testing.T) {
	ctx := withToolCallID(context.Background(), "call_42")
	if got := toolCallIDFrom(ctx); got != "call_42" {
		t.Fatalf("got %q", got)
	}
	if got := toolCallIDFrom(context.Background()); got != "" {
		t.Fatalf("empty ctx gave %q", got)
	}
}
