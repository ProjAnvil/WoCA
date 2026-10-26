package worker

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/projanvil/langchain-golang/core/messages"
	"github.com/rs/zerolog"
)

func TestFileSessionsRoundtrip(t *testing.T) {
	dir := t.TempDir()
	s := NewFileSessions(dir, zerolog.Nop())
	if s == nil {
		t.Fatal("NewFileSessions(writable dir) = nil, want a store")
	}
	in := []messages.Message{messages.Human("hello"), messages.AI("hi back")}
	if err := s.Save("sess-1", in, ""); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// The transcript lands in the shared JSONL layout under id+".jsonl".
	if _, err := os.Stat(filepath.Join(dir, "sess-1.jsonl")); err != nil {
		t.Fatalf("transcript file missing: %v", err)
	}
	out := s.Load("sess-1")
	if len(out) != len(in) {
		t.Fatalf("Load got %d messages, want %d", len(out), len(in))
	}
	for i := range in {
		if got, want := messages.Text(out[i]), messages.Text(in[i]); got != want {
			t.Errorf("msg %d text = %q, want %q", i, got, want)
		}
	}
}

func TestFileSessionsLoadUnknown(t *testing.T) {
	s := NewFileSessions(t.TempDir(), zerolog.Nop())
	if s == nil {
		t.Fatal("NewFileSessions(writable dir) = nil, want a store")
	}
	if got := s.Load("nope"); got != nil {
		t.Fatalf("Load(unknown) = %v, want nil", got)
	}
}

func TestFileSessionsEmptyDirDisables(t *testing.T) {
	if s := NewFileSessions("", zerolog.Nop()); s != nil {
		t.Fatalf(`NewFileSessions("") = %v, want nil (persistence disabled)`, s)
	}
}

func TestFileSessionsUnusableDirDisables(t *testing.T) {
	block := filepath.Join(t.TempDir(), "file.txt")
	if err := os.WriteFile(block, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := NewFileSessions(filepath.Join(block, "sub"), zerolog.Nop())
	if s != nil {
		t.Fatalf("NewFileSessions(dir under a file) = %v, want nil (persistence disabled)", s)
	}
}
