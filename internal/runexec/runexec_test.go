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
	if _, ok := RunExecutorFrom(context.Background()); ok {
		t.Fatal("RunExecutorFrom on a bare context must report false")
	}
	if _, ok := RunExecutorFrom(nil); ok {
		t.Fatal("RunExecutorFrom on nil ctx must report false")
	}
}
