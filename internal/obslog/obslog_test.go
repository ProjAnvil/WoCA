package obslog

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/projanvil/langchain-golang/core/messages"
)

func readLines(t *testing.T, path string) []Event {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	var out []Event
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var ev Event
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			t.Fatalf("parse line: %v", err)
		}
		out = append(out, ev)
	}
	return out
}

func TestStoreDisabledWritesNothing(t *testing.T) {
	dir := t.TempDir()
	s := New(dir, false)
	s.SetContext("/tmp/proj", "proj", "sess-1", "run-1")
	s.Log("info", "run_start", nil)
	s.DumpMessages([]messages.Message{messages.Human("hi")}, nil)
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("disabled store wrote files: %v", entries)
	}
}

func TestStoreSessionTraceAndDump(t *testing.T) {
	dir := t.TempDir()
	s := New(dir, true)
	s.SetContext("/tmp/my project", "my project", "sess-42", "run-7")

	s.Log("info", "run_start", map[string]any{"prompt": "hello"})
	s.Log("debug", "graph_node", map[string]any{"graph": "agent", "node": "model", "phase": "start"})
	msgs := []messages.Message{
		messages.Human("hello"),
		messages.AI("world"),
	}
	s.DumpMessages(msgs, map[string]any{"turns": 1})

	// Main log + session trace both exist; the session trace carries the
	// workspace/session/run tags on every line.
	if _, err := os.Stat(filepath.Join(dir, "woca.log")); err != nil {
		t.Fatalf("main log missing: %v", err)
	}
	wsDir := workspaceDir("/tmp/my project")
	trace := filepath.Join(dir, wsDir, "sess-42.jsonl")
	events := readLines(t, trace)
	if len(events) != 2 {
		t.Fatalf("session trace events = %d, want 2", len(events))
	}
	for _, ev := range events {
		if ev.Workspace != "/tmp/my project" || ev.Session != "sess-42" || ev.Run != "run-7" {
			t.Fatalf("event tags = %+v", ev)
		}
	}
	if events[0].Event != "run_start" || events[0].Data["prompt"] != "hello" {
		t.Fatalf("first event = %+v", events[0])
	}

	// Message dump: full transcript JSON under the session directory.
	dumpPath := filepath.Join(dir, wsDir, "sess-42", "run-7.json")
	b, err := os.ReadFile(dumpPath)
	if err != nil {
		t.Fatalf("dump missing: %v", err)
	}
	var dump struct {
		Session  string             `json:"session"`
		Run      string             `json:"run"`
		State    map[string]any     `json:"state"`
		Messages []messages.Message `json:"messages"`
	}
	if err := json.Unmarshal(b, &dump); err != nil {
		t.Fatalf("parse dump: %v", err)
	}
	if dump.Session != "sess-42" || dump.Run != "run-7" || len(dump.Messages) != 2 {
		t.Fatalf("dump = %+v", dump)
	}
	if dump.State["turns"] != float64(1) {
		t.Fatalf("state summary = %+v", dump.State)
	}
}

func TestRotatingWriterKeepsNewest(t *testing.T) {
	dir := t.TempDir()
	w := &rotatingWriter{
		path:     filepath.Join(dir, "trace.log"),
		maxBytes: 64,
		keep:     3,
	}
	chunk := strings.Repeat("x", 30) + "\n" // 31 bytes; rotates every ~3 writes
	for i := 0; i < 12; i++ {
		if _, err := w.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"trace.log", "trace.log.1", "trace.log.2", "trace.log.3"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("%s missing after rotations: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "trace.log.4")); !os.IsNotExist(err) {
		t.Fatal("rotation kept more than keep=3 backups")
	}
	// The active file is still appendable after rotation.
	if _, err := w.Write([]byte("tail\n")); err != nil {
		t.Fatal(err)
	}
}

func TestObserveNodeSummarizesUpdates(t *testing.T) {
	dir := t.TempDir()
	s := New(dir, true)
	s.SetContext("/tmp/p", "p", "sess-1", "run-1")

	s.ObserveNode(NodeEvent{
		Graph: "agent", Node: "model", Phase: PhaseStart,
		Values: map[string]any{"messages": []messages.Message{messages.Human("hi")}},
	})
	s.ObserveNode(NodeEvent{
		Graph: "agent", Node: "model", Phase: PhaseEnd, DurationMS: 1.5,
		Update: map[string]any{"messages": []messages.Message{
			messages.AI("done"),
			{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{{ID: "1", Name: "Read"}}},
		}},
	})

	events := readLines(t, filepath.Join(dir, workspaceDir("/tmp/p"), "sess-1.jsonl"))
	if len(events) != 2 {
		t.Fatalf("events = %d", len(events))
	}
	if events[0].Data["messages"] != float64(1) {
		t.Fatalf("start summary = %+v", events[0].Data)
	}
	upd, _ := events[1].Data["update"].([]any)
	if len(upd) != 2 {
		t.Fatalf("end summary = %+v", events[1].Data["update"])
	}
	toolMsg, _ := upd[1].(map[string]any)
	calls, _ := toolMsg["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("tool call not summarized: %+v", toolMsg)
	}
}

func TestWorkspaceDirSanitized(t *testing.T) {
	a := workspaceDir("/home/u/My Project!")
	b := workspaceDir("/home/u/My Project!")
	if a != b {
		t.Fatalf("same workspace maps to different dirs: %q vs %q", a, b)
	}
	if strings.ContainsAny(a, " !") {
		t.Fatalf("unsafe characters in dir name: %q", a)
	}
	if c := workspaceDir("/home/u/other"); c == a {
		t.Fatal("different workspaces collide")
	}
}
