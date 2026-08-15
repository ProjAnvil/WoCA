package session

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/projanvil/langchain-golang/core/messages"
)

func TestSaveStateLoadStateRoundTrip(t *testing.T) {
	want := State{
		Messages: []messages.Message{
			messages.System("you are a helpful assistant"),
			messages.Human("hello"),
			messages.AI("hi there"),
		},
		PlanMode: true,
		Todos: []Todo{
			{Content: "write tests", Status: "in_progress", ActiveForm: "writing tests"},
			{Content: "ship it", Status: "pending"},
		},
		Tasks: []Task{
			{ID: "task-1", Subject: "subject one", Description: "description one", Status: "done"},
			{ID: "task-2", Subject: "subject two", Description: "", Status: "open"},
		},
	}

	path := filepath.Join(t.TempDir(), "state.json")
	if err := SaveState(path, want); err != nil {
		t.Fatalf("SaveState() error = %v", err)
	}

	got, err := LoadState(path)
	if err != nil {
		t.Fatalf("LoadState() error = %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("LoadState() mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

func TestLoadStateMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.json")
	got, err := LoadState(path)
	if err != nil {
		t.Fatalf("LoadState() error = %v, want nil", err)
	}
	if !reflect.DeepEqual(got, State{}) {
		t.Fatalf("LoadState() = %#v, want empty State", got)
	}
}

func TestSaveStateCreatesParentDirectories(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "dir", "state.json")
	if err := SaveState(path, State{PlanMode: true}); err != nil {
		t.Fatalf("SaveState() error = %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("snapshot file not created: %v", err)
	}
}
