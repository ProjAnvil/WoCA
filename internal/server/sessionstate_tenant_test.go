package server

// Tenant threading through session-state writes: the busy rows a run
// produces must be written under the run's tenant (the session_state table
// is FORCE RLS-scoped on postgres, so an unscoped write lands 'local' and
// the owning tenant's listings never see the session as busy). The ctx
// capture store below asserts the context that actually reaches the store
// carries the tenant at every call site class: local-run marks, dispatched
// enqueue marks, and the terminal clear.

import (
	"context"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/projanvil/langchain-golang/core/messages"
	"github.com/projanvil/woca/internal/platform/tenancy"
	"github.com/projanvil/woca/internal/wocadb"
	"github.com/rs/zerolog"
)

// ctxCaptureState records the context of every store call.
type ctxCaptureState struct {
	mu    sync.Mutex
	set   []context.Context
	clear []context.Context
}

func (c *ctxCaptureState) Set(ctx context.Context, _ wocadb.SessionState) {
	c.mu.Lock()
	c.set = append(c.set, ctx)
	c.mu.Unlock()
}

func (c *ctxCaptureState) ClearIfRun(ctx context.Context, _, _ string) bool {
	c.mu.Lock()
	c.clear = append(c.clear, ctx)
	c.mu.Unlock()
	return true
}

func (c *ctxCaptureState) Delete(context.Context, string) {}

func (c *ctxCaptureState) Snapshot(context.Context) map[string]wocadb.SessionState {
	return map[string]wocadb.SessionState{}
}

func (c *ctxCaptureState) waitForSet(t *testing.T) context.Context {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		c.mu.Lock()
		n := len(c.set)
		var ctx context.Context
		if n > 0 {
			ctx = c.set[0]
		}
		c.mu.Unlock()
		if n > 0 {
			return ctx
		}
		select {
		case <-deadline:
			t.Fatal("session-state Set never fired")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func (c *ctxCaptureState) waitForClear(t *testing.T) context.Context {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		c.mu.Lock()
		n := len(c.clear)
		var ctx context.Context
		if n > 0 {
			ctx = c.clear[0]
		}
		c.mu.Unlock()
		if n > 0 {
			return ctx
		}
		select {
		case <-deadline:
			t.Fatal("session-state ClearIfRun never fired")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// TestLocalRunSessionStateMarkCarriesTenant: a locally executing run marks
// the session busy under its own (request-derived) run context.
func TestLocalRunSessionStateMarkCarriesTenant(t *testing.T) {
	s := New(Config{
		Agent: newScriptedAgent(t, "reply"), Static: fstest.MapFS{},
		Logger: zerolog.Nop(), SessionDir: t.TempDir(), WorkDir: t.TempDir(),
		RunLedger: openRunLedger(t),
	})
	capture := &ctxCaptureState{}
	s.state = capture

	ctx := tenancy.NewContext(t.Context(), tenancy.Context{TenantID: "tenant-a", UserID: "u1"})
	run, _, _, err := s.startRun(ctx, "s-mark", messages.Human("hi"), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	got := capture.waitForSet(t)
	tc, ok := tenancy.FromContext(got)
	if !ok || tc.TenantID != "tenant-a" {
		t.Fatalf("local-run mark ctx tenant = %+v ok=%v, want tenant-a", tc, ok)
	}
	waitRunSettled(t, s, run, "s-mark")
}

// TestLocalRunSessionStateClearCarriesTenant: the terminal clear (an
// onFinish hook that can fire after the run context ended) still carries
// the run's tenant — WithoutCancel sheds cancellation, not values.
func TestLocalRunSessionStateClearCarriesTenant(t *testing.T) {
	s := New(Config{
		Agent: newScriptedAgent(t, "reply"), Static: fstest.MapFS{},
		Logger: zerolog.Nop(), SessionDir: t.TempDir(), WorkDir: t.TempDir(),
	})
	capture := &ctxCaptureState{}
	s.state = capture

	ctx := tenancy.NewContext(t.Context(), tenancy.Context{TenantID: "tenant-a"})
	run, _, _, err := s.startRun(ctx, "s-clear", messages.Human("hi"), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	got := capture.waitForClear(t)
	tc, ok := tenancy.FromContext(got)
	if !ok || tc.TenantID != "tenant-a" {
		t.Fatalf("terminal clear ctx tenant = %+v ok=%v, want tenant-a", tc, ok)
	}
	waitRunSettled(t, s, run, "s-clear")
}

// TestDispatchSessionStateMarkCarriesTenant: a queue-dispatched run's busy
// mark rides the tenant-carrying runCtx (rootCtx liveness + request
// tenant), the same context the enqueue used.
func TestDispatchSessionStateMarkCarriesTenant(t *testing.T) {
	s, _, _ := newDispatchServer(t, false)
	capture := &ctxCaptureState{}
	s.state = capture

	ctx := tenancy.NewContext(t.Context(), tenancy.Context{TenantID: "tenant-b", UserID: "u2"})
	if _, _, _, err := s.enqueueRun(ctx, "s-dispatch-mark", messages.Human("queue me"), "", nil); err != nil {
		t.Fatal(err)
	}
	got := capture.waitForSet(t)
	tc, ok := tenancy.FromContext(got)
	if !ok || tc.TenantID != "tenant-b" {
		t.Fatalf("dispatch mark ctx tenant = %+v ok=%v, want tenant-b", tc, ok)
	}
}
