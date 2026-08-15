package tokens

import (
	"testing"

	"github.com/projanvil/langchain-golang/core/messages"
)

func TestEstimateText(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"hello world", 2},
		{"hello world this is a test", 6},
		{"x := foo(bar)", 7}, // x : = foo ( bar ) = 7
		{"if err != nil { return }", 8},
		{"你好世界", 4}, // 4 CJK chars
	}
	for _, c := range cases {
		if got := EstimateText(c.in); got != c.want {
			t.Errorf("EstimateText(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestEstimateMessages(t *testing.T) {
	msgs := []messages.Message{
		messages.Human("hello world"),
		messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
			{Name: "Bash", Args: map[string]any{"command": "ls -la"}},
		}},
	}
	if got := Estimate(msgs); got <= 0 {
		t.Fatalf("Estimate = %d", got)
	}
}

func TestEstimateMonotonic(t *testing.T) {
	short := EstimateText("abc")
	long := EstimateText("abc def ghi jkl mno")
	if long <= short {
		t.Fatalf("longer text should estimate more tokens: %d vs %d", short, long)
	}
}

func FuzzEstimateText(f *testing.F) {
	f.Add("hello world")
	f.Add("if err != nil { return }")
	f.Add("")
	f.Fuzz(func(t *testing.T, s string) {
		if n := EstimateText(s); n < 0 {
			t.Fatalf("negative estimate %d for %q", n, s)
		}
	})
}
