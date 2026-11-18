package agent

// T5 Part A RED repro (e2e hitl "two parallel child approvals" step 2): a
// session-grant answer on ONE child's card must collapse the sibling card
// (same rule key) and the run must COMPLETE. The e2e wedges here at HEAD
// (answer 200, stack closes, run never finishes); this is the hermetic twin.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"
	"github.com/projanvil/langchain-golang/core/runnables"
	"github.com/projanvil/langchain-golang/core/schema"
	coretools "github.com/projanvil/langchain-golang/core/tools"

	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/tools"
)

// pkgFileChildModel writes pkg/<a|b>.txt (same grant directory → same session
// rule key "file:pkg/*") then reports done — the hermetic twin of the e2e's
// npm-install children whose bash rule keys collapse.
type pkgFileChildModel struct{}

func (m *pkgFileChildModel) pick(input []messages.Message) messages.Message {
	prompt := ""
	for _, msg := range input {
		if msg.Role == messages.RoleTool {
			return messages.AI("sub done")
		}
		if msg.Role == messages.RoleHuman && prompt == "" {
			prompt = msg.Content
		}
	}
	file := "pkg/a.txt"
	if strings.Contains(prompt, "FILE-B") {
		file = "pkg/b.txt"
	}
	return writeCall("w-"+filepath.Base(file), file, "x")
}

func (m *pkgFileChildModel) Invoke(_ context.Context, input []messages.Message, _ ...runnables.Option) (messages.Message, error) {
	return m.pick(input), nil
}

func (m *pkgFileChildModel) Batch(ctx context.Context, inputs [][]messages.Message, opts ...runnables.Option) ([]messages.Message, error) {
	out := make([]messages.Message, len(inputs))
	for i, in := range inputs {
		resp, _ := m.Invoke(ctx, in, opts...)
		out[i] = resp
	}
	return out, nil
}

func (m *pkgFileChildModel) Stream(_ context.Context, input []messages.Message, _ ...runnables.Option) (runnables.Stream[messages.Message], error) {
	return runnables.NewSliceStream([]messages.Message{m.pick(input)}), nil
}

func (m *pkgFileChildModel) InputSchema() schema.Schema  { return schema.Schema{"type": "array"} }
func (m *pkgFileChildModel) OutputSchema() schema.Schema { return schema.Schema{"type": "object"} }
func (m *pkgFileChildModel) BindTools([]coretools.Tool) (language.ChatModel, error) {
	return m, nil
}
func (m *pkgFileChildModel) Capabilities() language.ChatModelCapabilities {
	return language.ChatModelCapabilities{ToolCalling: true, Streaming: true}
}

// TestGrantCollapseCompletesRun: the server's grant answer shape (the
// collapsed map the /answer path builds) must drive the two-child pause to
// completion — the granted child AND the collapsed sibling both proceed, both
// reports land, and the run finishes.
func TestGrantCollapseCompletesRun(t *testing.T) {
	workdir := t.TempDir()
	a, err := New(Config{
		Model:          &n1ParentModel{},
		SubagentModel:  &pkgFileChildModel{},
		Registry:       tools.NewRegistry(workdir),
		SystemPrompt:   "t",
		PermissionMode: config.ModeDefault,
		WorkDir:        workdir,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	thread := "thread-t5-grant-collapse"
	raw, _ := pumpStream(t, a.Stream(ctx, []messages.Message{messages.Human("go")}, thread))
	if len(raw) != 2 {
		t.Fatalf("pauses = %d, want 2 (both children's Write asks)", len(raw))
	}

	// Build the grant answer exactly like the server's /answer collapse: the
	// granted card's decision plus an allow-once for the same-rule-key
	// sibling, keyed by each interrupt's NS.
	resume := map[string]any{}
	var grantNS, otherNS string
	var grantAsk, otherAsk Ask
	for _, in := range raw {
		req := DecodeInterruptValue(in.Value).(ApprovalRequest)
		if grantNS == "" {
			grantNS, grantAsk = in.NS, req.Asks[0]
			continue
		}
		otherNS, otherAsk = in.NS, req.Asks[0]
	}
	if grantAsk.SessionRuleKey == "" || grantAsk.SessionRuleKey != otherAsk.SessionRuleKey {
		t.Fatalf("rule keys must match for the collapse: %q vs %q", grantAsk.SessionRuleKey, otherAsk.SessionRuleKey)
	}
	resume[grantNS] = ApprovalAnswer{Decisions: map[string]AskDecision{
		grantAsk.CallID: {Outcome: OutcomeAllow, Grant: GrantSession},
	}}
	resume[otherNS] = ApprovalAnswer{Decisions: map[string]AskDecision{
		otherAsk.CallID: {Outcome: OutcomeAllow},
	}}

	intrs2, final := pumpStream(t, a.ResumeStream(ctx, thread, resume))
	if len(intrs2) != 0 {
		t.Fatalf("run re-paused after the collapsed grant answer: %#v", DecodeInterruptValue(intrs2[0].Value))
	}
	if !toolContentContains(final, "sub done") {
		t.Fatalf("children's reports missing: %v", final)
	}
	for _, f := range []string{"pkg/a.txt", "pkg/b.txt"} {
		if _, err := os.Stat(filepath.Join(workdir, f)); err != nil {
			t.Fatalf("approved write %s did not execute: %v", f, err)
		}
	}
}
