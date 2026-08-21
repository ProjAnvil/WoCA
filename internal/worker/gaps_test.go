package worker

// Gap tests: default worker id, the prompt path (config snapshot → model
// turn → delta), mirror failure logging, the Run loop's error backoff, and
// snapshot parsing.

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"
	"github.com/rs/zerolog"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/runstate"
	"github.com/projanvil/woca/internal/tools"
	"github.com/projanvil/woca/internal/wocadb"
)

func TestNewDefaultsWorkerID(t *testing.T) {
	if w := New(Config{Logger: zerolog.Nop()}); w == nil || w.id == "" {
		t.Fatal("New should mint a default worker id")
	}
}

func TestWorkerExecutesPromptFromSnapshot(t *testing.T) {
	w, ledger, _ := newWorkerFixture(t, "worker-reply")
	if err := ledger.Enqueue(context.Background(), runstate.Run{
		RunID: "r-prompt", SessionID: "s-prompt", ThreadID: "th-prompt",
		ConfigSnapshot: `{"prompt":"say something"}`,
	}); err != nil {
		t.Fatal(err)
	}
	if err := w.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	events, err := ledger.Replay(context.Background(), "r-prompt", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]bool{}
	for _, ev := range events {
		kinds[ev.Kind] = true
	}
	if !kinds["run_started"] || !kinds["delta"] || !kinds["done"] {
		t.Fatalf("mirrored kinds = %v", kinds)
	}
	for _, ev := range events {
		if ev.Kind == "delta" && ev.Payload == "" {
			t.Fatal("delta event carries no text")
		}
	}
}

func TestExtractPromptBranches(t *testing.T) {
	if got := extractPrompt(""); got != "" {
		t.Fatalf("empty snapshot = %q", got)
	}
	if got := extractPrompt("{not json"); got != "" {
		t.Fatalf("bad json = %q", got)
	}
	if got := extractPrompt(`{"prompt":"hello"}`); got != "hello" {
		t.Fatalf("prompt = %q", got)
	}
}

func TestEmitAndMirrorDegradedPaths(t *testing.T) {
	_, ledger, _ := newWorkerFixture(t, "x")
	w := New(Config{Ledger: ledger, Logger: zerolog.Nop(), ID: "w"})
	// Unmarshalable payload degrades to {}; the append targets an unknown
	// run, so the mirror failure is logged without crashing.
	w.emitAndMirror(context.Background(), "no-such-run", "delta", map[string]any{"ch": make(chan int)})
}

func TestRunLoopBacksOffOnStepError(t *testing.T) {
	db, err := wocadb.Open("sqlite:" + filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := runstate.New(db)
	if err != nil {
		t.Fatal(err)
	}
	a, err := agent.New(agent.Config{
		Model:          language.NewFakeChatModel(language.WithResponses(messages.AI("x"))),
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "t",
		PermissionMode: config.ModeBypass,
	})
	if err != nil {
		t.Fatal(err)
	}
	// A closed ledger makes every claim fail; the loop must back off for
	// the one-second window and exit cleanly on cancellation.
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	w := New(Config{Ledger: ledger, Agent: a, Logger: zerolog.Nop(), ID: "w"})
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(1500*time.Millisecond, cancel)
	start := time.Now()
	if err := w.Run(ctx); err != nil {
		t.Fatalf("Run = %v", err)
	}
	if time.Since(start) < time.Second {
		t.Fatal("Run should have gone through the backoff window")
	}
}
