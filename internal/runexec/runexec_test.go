package runexec

import (
	"context"
	"testing"

	"github.com/projanvil/woca/internal/platform/executor"
)

func TestWithRunExecutorRoundTrip(t *testing.T) {
	x := executor.NewLocal()
	ctx := WithRunExecutor(context.Background(), x)

	got, ok := RunExecutorFrom(ctx)
	if !ok {
		t.Fatal("RunExecutorFrom: injected executor not found")
	}
	if got != x {
		t.Fatalf("RunExecutorFrom: got %p, want %p", got, x)
	}
}

func TestRunExecutorFromEmpty(t *testing.T) {
	for _, tc := range []struct {
		name string
		ctx  context.Context
	}{
		{name: "bare", ctx: t.Context()},
		{name: "nil"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := RunExecutorFrom(tc.ctx); ok {
				t.Fatal("RunExecutorFrom on an empty context must report false")
			}
		})
	}
}
