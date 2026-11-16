package agent

// Same-batch file-mutation serialization (deepagents borrow, woca's stance):
// deepagents rejects a model batch carrying two filesystem writes against
// overlapping state; woca runs batches concurrently, so two writes to ONE
// path raced — both read the same base content and the later write silently
// clobbered the earlier edit (a lost update the model never hears about).
// The tools node therefore serializes same-path mutations in call order;
// disjoint calls keep the historical per-call parallelism.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/tools"
)

func newBatchMutationAgent(t *testing.T, workdir string, calls ...messages.ToolCall) *Agent {
	t.Helper()
	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(
			messages.Message{Role: messages.RoleAI, ToolCalls: calls},
			messages.AI("done"),
		),
	)
	a, err := New(Config{
		Model:          model,
		Registry:       tools.NewRegistry(workdir),
		SystemPrompt:   "t",
		PermissionMode: config.ModeBypass,
		WorkDir:        workdir,
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// Two Edits on one file in one batch must BOTH land: the second edit applies
// to the first edit's result, not to a stale concurrent read of the base.
func TestSameFileEditsSerializeWithinBatch(t *testing.T) {
	workdir := t.TempDir()
	target := filepath.Join(workdir, "notes.txt")
	if err := os.WriteFile(target, []byte("alpha\nbeta\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	edit := func(id, old, new string) messages.ToolCall {
		return messages.ToolCall{ID: id, Name: "Edit", Args: map[string]any{
			"file_path": "notes.txt", "old_string": old, "new_string": new,
		}}
	}
	a := newBatchMutationAgent(t, workdir,
		edit("e1", "alpha", "ALPHA"),
		edit("e2", "beta", "BETA"),
	)
	out, err := a.Invoke(t.Context(), []messages.Message{messages.Human("fix both lines")}, "batch-edit-t1")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); got != "ALPHA\nBETA\n" {
		t.Fatalf("file after batch = %q, want both edits applied", got)
	}
	var edited int
	for _, m := range out {
		if m.Role == messages.RoleTool && strings.Contains(m.Content, "Edited notes.txt") {
			edited++
		}
	}
	if edited != 2 {
		t.Fatalf("successful edit results = %d, want 2 (transcript: %+v)", edited, out)
	}
}

// Write followed by Edit on the same path in one batch is a valid model
// pattern (create then refine): the edit must see the just-written content.
func TestWriteThenEditSameFileSerializeWithinBatch(t *testing.T) {
	workdir := t.TempDir()
	a := newBatchMutationAgent(t, workdir,
		messages.ToolCall{ID: "w1", Name: "Write", Args: map[string]any{
			"file_path": "fresh.go", "content": "package main\n\nfunc main() {}\n",
		}},
		messages.ToolCall{ID: "e1", Name: "Edit", Args: map[string]any{
			"file_path": "fresh.go", "old_string": "func main() {}", "new_string": "func main() { println(\"hi\") }",
		}},
	)
	if _, err := a.Invoke(t.Context(), []messages.Message{messages.Human("make the file")}, "batch-we-t1"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(workdir, "fresh.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `println("hi")`) {
		t.Fatalf("file after batch = %q, want the edit applied on top of the write", data)
	}
}

// Disjoint paths in one batch are independent: both writes land regardless
// of order (pins that serialization keys on the path, not on the batch).
func TestDifferentFilesStayIndependentWithinBatch(t *testing.T) {
	workdir := t.TempDir()
	a := newBatchMutationAgent(t, workdir,
		messages.ToolCall{ID: "w1", Name: "Write", Args: map[string]any{"file_path": "a.txt", "content": "A"}},
		messages.ToolCall{ID: "w2", Name: "Write", Args: map[string]any{"file_path": "b.txt", "content": "B"}},
	)
	if _, err := a.Invoke(t.Context(), []messages.Message{messages.Human("two files")}, "batch-2f-t1"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a.txt", "b.txt"} {
		data, err := os.ReadFile(filepath.Join(workdir, name))
		if err != nil || len(data) == 0 {
			t.Fatalf("%s after batch: %q (%v)", name, data, err)
		}
	}
}
