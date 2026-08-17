package obslog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/projanvil/langchain-golang/core/messages"

)

func TestStoreToggleAndContext(t *testing.T) {
	root := t.TempDir()
	s := New(root, false)

	if s.Root() != root {
		t.Fatalf("Root = %q", s.Root())
	}
	if s.Enabled() {
		t.Fatal("starts disabled")
	}

	// Runtime enable writes a marker; disable stops it.
	s.SetEnabled(true)
	if !s.Enabled() {
		t.Fatal("should be enabled")
	}
	s.SetContext("ws", "wsname", "sess-1", "turn-2")
	s.Log("evt", "detail", map[string]any{"k": "v"})
	s.SetEnabled(false)
	if s.Enabled() {
		t.Fatal("should be disabled")
	}

	// LockOpen pins the log on: disable attempts are ignored.
	s.LockOpen()
	if !s.Enabled() {
		t.Fatal("locked open should be enabled")
	}
	s.SetEnabled(false)
	if !s.Enabled() {
		t.Fatal("locked store cannot be disabled")
	}

	// Nil store is safe.
	var nilStore *Store
	nilStore.SetEnabled(true)
	nilStore.LockOpen()
	nilStore.SetContext("a", "b", "c", "d")
	nilStore.Log("evt", "", nil)
	nilStore.DumpMessages(nil, nil)
	nilStore.ObserveNode(NodeEvent{})
	nilStore.LogUsage("m", messages.UsageMetadata{TotalTokens: 5})
	if nilStore.Enabled() || nilStore.Root() != "" {
		t.Fatal("nil store misbehaves")
	}
}

func TestDumpAndUsageWhenEnabled(t *testing.T) {
	root := t.TempDir()
	s := New(root, true)
	s.SetContext("ws", "name", "sess", "run")
	s.DumpMessages([]messages.Message{
		messages.System("sys"),
		messages.Human("hello"),
		messages.AI("hi"),
	}, map[string]any{"summary": true})
	s.ObserveNode(NodeEvent{Node: "node-a", Phase: "end", DurationMS: 3})
	s.LogUsage("claude-sonnet-4-5", messages.UsageMetadata{InputTokens: 10, OutputTokens: 5, TotalTokens: 15})
	// Zero usage is skipped.
	s.LogUsage("m", messages.UsageMetadata{})

	// The enabled log directory now contains the JSONL files.
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("no log files written")
	}
	// Data lands under date-scoped files.
	found := false
	var data []byte
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err = os.ReadFile(filepath.Join(root, e.Name()))
		if err == nil && len(data) > 0 {
			found = true
		}
	}
	if !found {
		t.Fatal("log files are empty")
	}
}

func TestRotationBySize(t *testing.T) {
	root := t.TempDir()
	s := New(root, true)
	s.maxBytes = 64
	s.keep = 2
	s.SetContext("ws", "wsname", "sess", "run")
	// Write enough events to trip the size rotation several times.
	for i := 0; i < 40; i++ {
		s.Log("info", "event", map[string]any{"seq": i, "pad": strings.Repeat("x", 20)})
	}
	// At least one rotated file exists (path.1) and the primary file is
	// present.
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	rotated := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".1") || strings.HasSuffix(e.Name(), ".2") {
			rotated++
		}
	}
	if rotated == 0 {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("no rotated files; entries: %v", names)
	}
}
