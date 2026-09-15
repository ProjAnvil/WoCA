package agent

import (
	"errors"
	"testing"

	lgtypes "github.com/projanvil/langchain-golang/langgraph/types"
)

// interruptFromError extracts the pending interrupt from a paused-run error
// (*lgtypes.GraphInterrupt), failing the test when the run did not pause.
func interruptFromError(t *testing.T, err error) lgtypes.Interrupt {
	t.Helper()
	gi, ok := errors.AsType[*lgtypes.GraphInterrupt](err)
	if !ok {
		t.Fatalf("expected interrupt, got %v", err)
	}
	gi.Interrupt.Value = decodeInterruptPayload(gi.Interrupt.Value)
	return gi.Interrupt
}
