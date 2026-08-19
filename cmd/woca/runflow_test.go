package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/projanvil/langchain-golang/core/messages"
	"github.com/projanvil/woca/internal/agentbuild"
	"github.com/projanvil/woca/internal/session"
)

func TestRunOneShotSuccess(t *testing.T) {
	isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	url := fakeOpenAI(t, "Hello from the model")
	opts := baseOpts(t, url)
	opts.Prompt = "say hi"

	var out, errBuf bytes.Buffer
	if err := run(t.Context(), opts, strings.NewReader(""), &out, &errBuf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Hello from the model") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestRunOneShotJSONOutput(t *testing.T) {
	isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	url := fakeOpenAI(t, "structured")
	opts := baseOpts(t, url)
	opts.Prompt = "say hi"
	opts.OutputFormat = "json"

	var out, errBuf bytes.Buffer
	if err := run(t.Context(), opts, strings.NewReader(""), &out, &errBuf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"result"`) || !strings.Contains(out.String(), "structured") {
		t.Fatalf("json output = %q", out.String())
	}
}

func TestRunOneShotStreamJSONOutput(t *testing.T) {
	isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	url := fakeOpenAI(t, "streamed")
	opts := baseOpts(t, url)
	opts.Prompt = "say hi"
	opts.OutputFormat = "stream-json"

	var out, errBuf bytes.Buffer
	if err := run(t.Context(), opts, strings.NewReader(""), &out, &errBuf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"type"`) {
		t.Fatalf("stream output = %q", out.String())
	}
}

func TestRunOneShotBadOutputFormat(t *testing.T) {
	isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	url := fakeOpenAI(t, "x")
	opts := baseOpts(t, url)
	opts.Prompt = "say hi"
	opts.OutputFormat = "yaml"

	var out, errBuf bytes.Buffer
	if err := run(t.Context(), opts, strings.NewReader(""), &out, &errBuf); err == nil {
		t.Fatal("expected output-format error")
	}
}

func TestRunOneShotModelError(t *testing.T) {
	isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	opts := baseOpts(t, "http://127.0.0.1:1") // connection refused
	opts.Prompt = "say hi"

	var out, errBuf bytes.Buffer
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if err := run(ctx, opts, strings.NewReader(""), &out, &errBuf); err == nil {
		t.Fatal("expected model error")
	}
}

func TestRunBadLogLevel(t *testing.T) {
	opts := agentbuild.Options{LogLevel: "bogus", Prompt: "x"}
	var out, errBuf bytes.Buffer
	if err := run(t.Context(), opts, strings.NewReader(""), &out, &errBuf); err == nil {
		t.Fatal("expected log-level error")
	}
}

func TestRunResumeLastSession(t *testing.T) {
	home := isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	url := fakeOpenAI(t, "hi")

	dir := filepath.Join(home, ".woca")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	st := session.State{
		Messages: []messages.Message{messages.System("sys"), messages.Human("earlier"), messages.AI("answer")},
		PlanMode: true,
	}
	if err := session.SaveState(filepath.Join(dir, "last-session.jsonl"), st); err != nil {
		t.Fatal(err)
	}

	opts := baseOpts(t, url)
	opts.Resume = "last"
	var out, errBuf bytes.Buffer
	// Stdin at EOF: the REPL exits immediately after restoring history.
	if err := run(t.Context(), opts, strings.NewReader(""), &out, &errBuf); err != nil {
		t.Fatal(err)
	}
}

func TestRunResumeExplicitPath(t *testing.T) {
	isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	url := fakeOpenAI(t, "hi")
	path := filepath.Join(t.TempDir(), "session.jsonl")
	// Legacy transcript (one message per line): LoadState decodes the first
	// line as an empty State, then run() falls back to session.Load.
	if err := session.Save(path, []messages.Message{messages.Human("legacy hi")}); err != nil {
		t.Fatal(err)
	}

	opts := baseOpts(t, url)
	opts.Resume = path
	var out, errBuf bytes.Buffer
	if err := run(t.Context(), opts, strings.NewReader(""), &out, &errBuf); err != nil {
		t.Fatal(err)
	}
}

func TestRunResumeMissing(t *testing.T) {
	isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	opts := baseOpts(t, fakeOpenAI(t, "hi"))
	opts.Resume = filepath.Join(t.TempDir(), "nope.jsonl")
	var out, errBuf bytes.Buffer
	if err := run(t.Context(), opts, strings.NewReader(""), &out, &errBuf); err == nil {
		t.Fatal("expected resume error")
	}
}

func TestRunResumeCheckpointMissing(t *testing.T) {
	isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	opts := baseOpts(t, fakeOpenAI(t, "hi"))
	opts.ResumeCheckpoint = "no-such-key"
	var out, errBuf bytes.Buffer
	if err := run(t.Context(), opts, strings.NewReader(""), &out, &errBuf); err == nil {
		t.Fatal("expected resume-checkpoint error")
	}
}

func TestRunREPLFlow(t *testing.T) {
	isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	url := fakeOpenAI(t, "REPL answer")
	opts := baseOpts(t, url)

	input := "/help\nhello there\n!echo repl-shell-ok\n/nosuchcmd\n/exit\n"
	var out, errBuf bytes.Buffer
	if err := run(t.Context(), opts, strings.NewReader(input), &out, &errBuf); err != nil {
		t.Fatal(err)
	}
	combined := out.String() + errBuf.String()
	if !strings.Contains(combined, "repl-shell-ok") {
		t.Fatalf("shell output missing: %q", combined)
	}
	if !strings.Contains(combined, "Commands:") {
		t.Fatalf("help output missing: %q", combined)
	}
	if !strings.Contains(combined, "unknown command /nosuchcmd") {
		t.Fatalf("unknown-command error missing: %q", combined)
	}
	if !strings.Contains(combined, "REPL answer") {
		t.Fatalf("model answer missing: %q", combined)
	}
}

func TestRunREPLQuit(t *testing.T) {
	isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	opts := baseOpts(t, fakeOpenAI(t, "x"))

	var out, errBuf bytes.Buffer
	if err := run(t.Context(), opts, strings.NewReader("/quit\n"), &out, &errBuf); err != nil {
		t.Fatal(err)
	}
}

func TestRunREPLSubmitError(t *testing.T) {
	isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	// Unreachable Model: the submit error path prints and keeps the loop.
	opts := baseOpts(t, "http://127.0.0.1:1")
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	var out, errBuf bytes.Buffer
	if err := run(ctx, opts, strings.NewReader("hello\n/exit\n"), &out, &errBuf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errBuf.String(), "woca:") {
		t.Fatalf("submit error output = %q", errBuf.String())
	}
}

func TestRunREPLInitPrompt(t *testing.T) {
	isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	url := fakeOpenAI(t, "init done")
	opts := baseOpts(t, url)
	opts.Workdir = t.TempDir()

	// /init hands back a prompt that is submitted as a full agent turn.
	var out, errBuf bytes.Buffer
	if err := run(t.Context(), opts, strings.NewReader("/init AGENTS.md\n/exit\n"), &out, &errBuf); err != nil {
		t.Fatal(err)
	}
}

func TestRunSessionStartHookFeedback(t *testing.T) {
	isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	url := fakeOpenAI(t, "ok")
	workdir := t.TempDir()
	opts := baseOpts(t, url)
	opts.Workdir = workdir

	// Project-level SessionStart hook with feedback output.
	hooksDir := filepath.Join(workdir, ".woca")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	hooksJSON := `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"echo session-start-feedback"}]}]}}`
	if err := os.WriteFile(filepath.Join(hooksDir, "settings.json"), []byte(hooksJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	var out, errBuf bytes.Buffer
	if err := run(t.Context(), opts, strings.NewReader("/exit\n"), &out, &errBuf); err != nil {
		t.Fatal(err)
	}
}

func TestRunREPLCommands(t *testing.T) {
	isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	url := fakeOpenAI(t, "cmd-ok")
	opts := baseOpts(t, url)

	input := strings.Join([]string{
		"/model other-model",
		"/thinking 2048",
		"/quota",
		"/theme dark",
		"/theme no-such-theme",
		"/vim",
		"/vim",
		"/settings",
		"/settings help",
		"/settings get provider",
		"/settings set provider openai",
		"/settings set memory-engine none",
		"/settings set no-such-key x",
		"/memory",
		"/undo",
		"/redo",
		"/cost",
		"/context",
		"/permissions",
		"/permissions bypassPermissions",
		"/status",
		"/clear",
		"/agents",
		"run the model please",
		"/exit",
	}, "\n") + "\n"
	var out, errBuf bytes.Buffer
	if err := run(t.Context(), opts, strings.NewReader(input), &out, &errBuf); err != nil {
		t.Fatal(err)
	}
	combined := out.String() + errBuf.String()
	for _, want := range []string{"cmd-ok", "woca", "provider"} {
		if !strings.Contains(combined, want) {
			t.Errorf("missing %q in REPL output", want)
		}
	}
}

func TestRunREPLDebugLog(t *testing.T) {
	home := isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	url := fakeOpenAI(t, "dbg")
	opts := baseOpts(t, url)
	opts.DebugLog = true

	var out, errBuf bytes.Buffer
	if err := run(t.Context(), opts, strings.NewReader("/exit\n"), &out, &errBuf); err != nil {
		t.Fatal(err)
	}
	// The observability log directory exists under the isolated home.
	if _, err := os.Stat(filepath.Join(home, ".woca", "logs")); err != nil {
		t.Fatalf("obs log dir: %v", err)
	}
}

func TestRunREPLModelFromSettingsAndMemory(t *testing.T) {
	isolateEnv(t)
	// No provider flag: the model factory resolves from live settings. The
	// default memory engine (ladym) is built so /memory lists it.
	url := fakeOpenAI(t, "mem-ok")
	opts := agentbuild.Options{APIKey: "k", BaseURL: url, Workdir: t.TempDir()}

	var out, errBuf bytes.Buffer
	if err := run(t.Context(), opts, strings.NewReader("/model other\n/memory\n/quit\n"), &out, &errBuf); err != nil {
		t.Fatal(err)
	}
}

func TestRunResumeCheckpointSuccess(t *testing.T) {
	isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	url := fakeOpenAIToolFlow(t, "resumed and approved")
	ckptDir := t.TempDir()

	// Run one: the model asks to write a file; non-interactive mode denies
	// and the turn completes, leaving the checkpoint file behind.
	opts := baseOpts(t, url)
	opts.Prompt = "write a note"
	opts.CheckpointDir = ckptDir
	var out, errBuf bytes.Buffer
	if err := run(t.Context(), opts, strings.NewReader(""), &out, &errBuf); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(ckptDir)
	if err != nil || len(entries) == 0 {
		t.Fatalf("no checkpoint persisted: %v (%d entries)", err, len(entries))
	}
	key := strings.TrimSuffix(entries[0].Name(), ".json")

	// Run two: resume the checkpoint, approve the write, JSON output. The
	// model only needs to produce the final text now.
	opts2 := baseOpts(t, fakeOpenAI(t, "resumed and approved"))
	opts2.CheckpointDir = ckptDir
	opts2.ResumeCheckpoint = key
	opts2.ResumeValue = "y"
	opts2.OutputFormat = "json"
	var out2, errBuf2 bytes.Buffer
	if err := run(t.Context(), opts2, strings.NewReader(""), &out2, &errBuf2); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out2.String(), "resumed and approved") {
		t.Fatalf("resume output = %q", out2.String())
	}
}

func TestRunResumeLegacyCorrupt(t *testing.T) {
	isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	path := filepath.Join(t.TempDir(), "bad.jsonl")
	// The first object decodes as an empty State; the second line is not a
	// message, so the legacy fallback load fails.
	if err := os.WriteFile(path, []byte("{}\nnot-json-at-all\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	opts := baseOpts(t, fakeOpenAI(t, "hi"))
	opts.Resume = path
	var out, errBuf bytes.Buffer
	if err := run(t.Context(), opts, strings.NewReader(""), &out, &errBuf); err == nil {
		t.Fatal("expected legacy resume error")
	}
}

func TestRunResumeDecodeError(t *testing.T) {
	isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	path := filepath.Join(t.TempDir(), "garbage.jsonl")
	// The file does not start with a JSON object, so LoadState's decode of
	// the first value fails.
	if err := os.WriteFile(path, []byte("garbage-not-json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	opts := baseOpts(t, fakeOpenAI(t, "hi"))
	opts.Resume = path
	var out, errBuf bytes.Buffer
	if err := run(t.Context(), opts, strings.NewReader(""), &out, &errBuf); err == nil {
		t.Fatal("expected resume decode error")
	}
}

func TestRunBuildAgentError(t *testing.T) {
	isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	opts := baseOpts(t, fakeOpenAI(t, "hi"))
	opts.Provider = "nope"
	opts.Prompt = "x"
	var out, errBuf bytes.Buffer
	if err := run(t.Context(), opts, strings.NewReader(""), &out, &errBuf); err == nil {
		t.Fatal("expected buildAgent error")
	}
}

func TestRunOneShotToolTurnsJSON(t *testing.T) {
	isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	// The model requests a Write; non-interactive mode denies it, producing
	// a tool result message, then finishes with text.
	url := fakeOpenAIToolFlow(t, "after tool")
	opts := baseOpts(t, url)
	opts.Prompt = "write a note"
	opts.OutputFormat = "json"

	var out, errBuf bytes.Buffer
	if err := run(t.Context(), opts, strings.NewReader(""), &out, &errBuf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "after tool") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestRunWebSearchBackendError(t *testing.T) {
	isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	// --search-api registers the WebSearch tool with the custom backend; the
	// model calls it and the unreachable search endpoint surfaces as an
	// error result (then the turn completes).
	url := fakeOpenAICustomToolFlow(t, "WebSearch", `{"query":"anything"}`, "search failed but ok")
	opts := baseOpts(t, url)
	opts.Prompt = "search the web"
	opts.SearchAPI = "http://127.0.0.1:1/{q}"

	var out, errBuf bytes.Buffer
	if err := run(t.Context(), opts, strings.NewReader(""), &out, &errBuf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "search failed but ok") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestRunWebSearchBackendSuccess(t *testing.T) {
	isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	// A working search endpoint: the WebSearch backend formats results.
	search := startHTTPServer(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"results":[{"title":"Answer","url":"https://example.com/a","snippet":"the answer"}]}`)
	})
	url := fakeOpenAICustomToolFlow(t, "WebSearch", `{"query":"meaning"}`, "searched fine")
	opts := baseOpts(t, url)
	opts.Prompt = "search the web"
	opts.SearchAPI = search + "/search?q={q}"

	var out, errBuf bytes.Buffer
	if err := run(t.Context(), opts, strings.NewReader(""), &out, &errBuf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "searched fine") {
		t.Fatalf("output = %q", out.String())
	}
}
