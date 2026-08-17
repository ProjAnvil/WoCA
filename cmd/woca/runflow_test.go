package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/session"
)

func TestRunOneShotSuccess(t *testing.T) {
	isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	url := fakeOpenAI(t, "Hello from the model")
	opts := baseOpts(t, url)
	opts.prompt = "say hi"

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
	opts.prompt = "say hi"
	opts.outputFormat = "json"

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
	opts.prompt = "say hi"
	opts.outputFormat = "stream-json"

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
	opts.prompt = "say hi"
	opts.outputFormat = "yaml"

	var out, errBuf bytes.Buffer
	if err := run(t.Context(), opts, strings.NewReader(""), &out, &errBuf); err == nil {
		t.Fatal("expected output-format error")
	}
}

func TestRunOneShotModelError(t *testing.T) {
	isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	opts := baseOpts(t, "http://127.0.0.1:1") // connection refused
	opts.prompt = "say hi"

	var out, errBuf bytes.Buffer
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if err := run(ctx, opts, strings.NewReader(""), &out, &errBuf); err == nil {
		t.Fatal("expected model error")
	}
}

func TestRunBadLogLevel(t *testing.T) {
	opts := options{logLevel: "bogus", prompt: "x"}
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
	opts.resume = "last"
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
	opts.resume = path
	var out, errBuf bytes.Buffer
	if err := run(t.Context(), opts, strings.NewReader(""), &out, &errBuf); err != nil {
		t.Fatal(err)
	}
}

func TestRunResumeMissing(t *testing.T) {
	isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	opts := baseOpts(t, fakeOpenAI(t, "hi"))
	opts.resume = filepath.Join(t.TempDir(), "nope.jsonl")
	var out, errBuf bytes.Buffer
	if err := run(t.Context(), opts, strings.NewReader(""), &out, &errBuf); err == nil {
		t.Fatal("expected resume error")
	}
}

func TestRunResumeCheckpointMissing(t *testing.T) {
	isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	opts := baseOpts(t, fakeOpenAI(t, "hi"))
	opts.resumeCheckpoint = "no-such-key"
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
	// Unreachable model: the submit error path prints and keeps the loop.
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
	opts.workdir = t.TempDir()

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
	opts.workdir = workdir

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
