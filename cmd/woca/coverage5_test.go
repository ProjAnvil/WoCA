package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/projanvil/langchain-golang/core/messages"
)

func TestRunBuildAgentError(t *testing.T) {
	isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	opts := baseOpts(t, fakeOpenAI(t, "hi"))
	opts.provider = "nope"
	opts.prompt = "x"
	var out, errBuf bytes.Buffer
	if err := run(t.Context(), opts, strings.NewReader(""), &out, &errBuf); err == nil {
		t.Fatal("expected buildAgent error")
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
	opts.resume = path
	var out, errBuf bytes.Buffer
	if err := run(t.Context(), opts, strings.NewReader(""), &out, &errBuf); err == nil {
		t.Fatal("expected resume decode error")
	}
}

func TestTuiCmdBuildAgentError(t *testing.T) {
	isolateEnv(t)
	// Valid log level but an invalid provider: the command wires the
	// console, fails building the agent, and returns before the TUI runs.
	if err := execCmd(t, "tui", "--provider", "nope"); err == nil {
		t.Fatal("expected buildAgent error")
	}
}

func TestMcpAuthSettingsOpenError(t *testing.T) {
	home := t.TempDir()
	blocker := filepath.Join(home, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	if err := execCmd(t, "--db", filepath.Join(blocker, "woca.db"), "mcp", "auth", "srv", "http://127.0.0.1:1/mcp"); err == nil {
		t.Fatal("expected settings open error")
	}
}

func TestWebStatusAndStopWithRecords(t *testing.T) {
	home := isolateEnv(t)
	dataDir := filepath.Join(home, ".woca")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeRecord := func(pid int, addr string) {
		t.Helper()
		rec := map[string]any{"pid": pid, "addr": addr, "started": time.Now().Format(time.RFC3339)}
		data, _ := json.Marshal(rec)
		if err := os.WriteFile(filepath.Join(dataDir, "web.json"), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// A record whose pid is this test process: status reports running.
	writeRecord(os.Getpid(), "127.0.0.1:3998")
	if err := execCmd(t, "web", "status"); err != nil {
		t.Fatal(err)
	}

	// A record pointing at a spawned sleep process: stop kills it.
	sleep := exec.Command("sleep", "30")
	if err := sleep.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sleep.Process.Kill(); _ = sleep.Wait() }()
	writeRecord(sleep.Process.Pid, "127.0.0.1:3998")
	if err := execCmd(t, "web", "stop"); err != nil {
		t.Fatal(err)
	}
}

// fakeOpenAICustomToolFlow streams one tool call for the given tool/args on
// the first request and plain text afterwards.
func fakeOpenAICustomToolFlow(t *testing.T, tool string, args string, final string) string {
	t.Helper()
	var calls int32
	return newFakeStreamServer(t, func() []map[string]any {
		if atomic.AddInt32(&calls, 1) == 1 {
			return []map[string]any{{
				"index": 0,
				"delta": map[string]any{
					"role": "assistant",
					"tool_calls": []map[string]any{{
						"index": 0, "id": "call_1", "type": "function",
						"function": map[string]any{"name": tool, "arguments": args},
					}},
				},
				"finish_reason": nil,
			}, {
				"index": 0, "delta": map[string]any{}, "finish_reason": "tool_calls",
			}}
		}
		return []map[string]any{{
			"index": 0, "delta": map[string]any{"role": "assistant", "content": final}, "finish_reason": nil,
		}, {
			"index": 0, "delta": map[string]any{}, "finish_reason": "stop",
		}}
	})
}

func TestRunWebSearchBackendError(t *testing.T) {
	isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	// --search-api registers the WebSearch tool with the custom backend; the
	// model calls it and the unreachable search endpoint surfaces as an
	// error result (then the turn completes).
	url := fakeOpenAICustomToolFlow(t, "WebSearch", `{"query":"anything"}`, "search failed but ok")
	opts := baseOpts(t, url)
	opts.prompt = "search the web"
	opts.searchAPI = "http://127.0.0.1:1/{q}"

	var out, errBuf bytes.Buffer
	if err := run(t.Context(), opts, strings.NewReader(""), &out, &errBuf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "search failed but ok") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestRunREPLModelFromSettingsAndMemory(t *testing.T) {
	isolateEnv(t)
	// No provider flag: the model factory resolves from live settings. The
	// default memory engine (ladym) is built so /memory lists it.
	url := fakeOpenAI(t, "mem-ok")
	opts := options{apiKey: "k", baseURL: url, workdir: t.TempDir()}

	var out, errBuf bytes.Buffer
	if err := run(t.Context(), opts, strings.NewReader("/model other\n/memory\n/quit\n"), &out, &errBuf); err != nil {
		t.Fatal(err)
	}
}

func TestBuildAgentSyncSkillsError(t *testing.T) {
	home := isolateEnv(t)
	// A claude-skills selection while ~/.woca/skills exists as a regular
	// file: the sync fails and is warned about.
	seedSettings(t,
		[2]string{"memory-engine", "none"},
		[2]string{"claude-skills", `["myutil"]`},
	)
	if err := os.MkdirAll(filepath.Join(home, ".woca"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".woca", "skills"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	ap, err := buildAgent(t.Context(), baseOpts(t, fakeOpenAI(t, "hi")), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer ap.pluginMgr.Close()
	defer ap.a.Close()
}

func TestConnectMCPListToolsFailure(t *testing.T) {
	// The helper server initializes fine but fails tools/list on demand.
	bin := buildMCPHelperServer(t)
	t.Setenv("HELPER_FAIL_LIST", "1")
	got, clients := connectMCP(t.Context(), testLogger(), []string{"helper=" + bin}, nil)
	if len(got) != 0 || len(clients) != 0 {
		t.Fatalf("failing list = %d tools %d clients", len(got), len(clients))
	}
}

func TestSaveLastSessionWriteError(t *testing.T) {
	home := isolateEnv(t)
	// last-session.jsonl exists as a directory: the snapshot write fails
	// and prints an error instead of crashing.
	dir := filepath.Join(home, ".woca", "last-session.jsonl")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	endSession(t.Context(), []messages.Message{messages.Human("x")}, nil)
}
