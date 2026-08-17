package session

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/projanvil/langchain-golang/core/messages"
)

func TestDefaultDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if got := DefaultDir(); got != filepath.Join(home, ".woca") {
		t.Fatalf("DefaultDir = %q", got)
	}
	t.Setenv("HOME", "")
	if got := DefaultDir(); got != "" {
		t.Fatalf("DefaultDir without home = %q", got)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "s.jsonl")
	msgs := []messages.Message{
		messages.System("sys"),
		messages.Human("hello"),
		messages.AI("hi there"),
	}
	if err := Save(path, msgs); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(msgs) {
		t.Fatalf("loaded %d messages, want %d", len(got), len(msgs))
	}
	if got[1].Content != "hello" || got[2].Content != "hi there" {
		t.Fatalf("contents = %+v", got)
	}

	// Load errors: missing file and malformed content.
	if _, err := Load(filepath.Join(t.TempDir(), "missing.jsonl")); err == nil {
		t.Error("expected missing-file error")
	}
	bad := filepath.Join(t.TempDir(), "bad.jsonl")
	if err := os.WriteFile(bad, []byte("not-json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(bad); err == nil {
		t.Error("expected decode error")
	}

	// Save into an unwritable location.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Save(filepath.Join(blocker, "s.jsonl"), msgs); err == nil {
		t.Error("expected mkdir failure")
	}
}

func TestSaveLoadStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "state.json")
	st := State{
		Messages: []messages.Message{messages.Human("hi")},
		PlanMode: true,
		Todos:    []Todo{{Content: "task", Status: "in_progress", ActiveForm: "tasking"}},
		Tasks:    []Task{{ID: "task-1", Subject: "s", Description: "d", Status: "done"}},
	}
	if err := SaveState(path, st); err != nil {
		t.Fatal(err)
	}
	got, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if !got.PlanMode || len(got.Todos) != 1 || len(got.Tasks) != 1 || len(got.Messages) != 1 {
		t.Fatalf("state = %+v", got)
	}

	// Missing file: empty state without error.
	if got, err := LoadState(filepath.Join(t.TempDir(), "nope.json")); err != nil || got.PlanMode {
		t.Fatalf("missing state = %+v %v", got, err)
	}
	// Malformed file: decode error.
	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte("{nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadState(bad); err == nil {
		t.Error("expected decode error")
	}
	// Unwritable location.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SaveState(filepath.Join(blocker, "state.json"), st); err == nil {
		t.Error("expected mkdir failure")
	}
	// A directory in place of the file fails at Create.
	if err := SaveState(t.TempDir(), st); err == nil {
		t.Error("expected create failure")
	}
}
