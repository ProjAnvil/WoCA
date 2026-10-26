// fakemodel is the scripted Anthropic-Messages-API stand-in for the
// sandboxlab e2e stack and the full-stack sandbox demo (compose profile
// "demo"): it answers POST {base}/messages with the SSE stream shape the
// langchain-golang anthropic partner consumes (partners/anthropic/
// stream.go).
//
// The script is selected per request: a recognized marker token in the
// conversation (the original user prompt persists across turns) picks a
// scripted scenario, and the number of accumulated tool_result blocks
// picks the turn inside it. Without any token the legacy two-turn
// sandboxlab script runs unchanged:
//
//   - a request WITHOUT a tool_result (the first turn) yields one
//     tool_use block calling the Write tool with {"file_path":"out.txt",
//     "content":"sandbox-side"} — the product the test asserts on the
//     shared workspace volume after the worker downloads it back;
//   - a request WITH a tool_result (the turn after the tool ran) yields
//     a plain text block, ending the run.
//
// Demo scenarios (make ent-sandbox-demo, spec §12):
//
//   - "woca-demo:build"    turn 1 Write out.txt, turn 2 Bash a python
//     heredoc that runs a unittest (test_ok.txt),
//     probes egress (net.txt: net:blocked expected —
//     sandbox network is none) and records the
//     container hostname (host.txt), turn 3 ends;
//   - "woca-demo:continue" turn 1 Bash reads round one's out.txt into
//     round2.txt and records host2.txt (a different
//     container id — sandboxes never survive a run);
//   - "woca-demo:long"     turn 1 Bash sleeps, then records whether the
//     image carries /marker — proves a hot options
//     change leaves running sandboxes on the old
//     image while new ones pick up the new one;
//   - "woca-demo:marker"   turn 1 Bash copies /marker to marker.txt —
//     the marker image is only present after the
//     hot backend-options change took effect.
//
// Single-file, stdlib-only, no woca imports — it builds inside its own
// tiny docker stage (see Dockerfile beside this file). The wire shape is
// pinned by the smoke tests below against the real anthropic client.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	flag.Parse()

	fmt.Fprintf(os.Stderr, "fakemodel: %s (scripted anthropic Messages SSE)\n", *addr)
	if err := http.ListenAndServe(*addr, newHandler()); err != nil {
		fmt.Fprintf(os.Stderr, "fakemodel: %v\n", err)
		os.Exit(1)
	}
}

// newHandler mounts the scripted Messages API plus a health probe.
func newHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	// The anthropic partner posts to {BaseURL}/messages; serve both the
	// bare and the /v1-prefixed forms so either base_url convention works.
	mux.HandleFunc("POST /messages", serve)
	mux.HandleFunc("POST /v1/messages", serve)
	return mux
}

// scriptStep is one model turn: a tool call (tool != "") or the final
// text that ends the run.
type scriptStep struct {
	tool string
	args string // JSON object text, the tool call's arguments
	text string // final-turn text (tool == "")
}

// Demo script tokens (matched against the raw request body — the token
// rides the first user message and therefore every later turn too).
const (
	tokenBuild    = "woca-demo:build"
	tokenContinue = "woca-demo:continue"
	tokenLong     = "woca-demo:long"
	tokenMarker   = "woca-demo:marker"
)

// buildScript writes the round-one product, then (turn two) runs the
// sandbox-side checks inside one python heredoc: a unittest pass flag,
// an egress probe (network=none must refuse) and the container identity.
const buildScript = `python3 - <<'PYEOF'
import socket, subprocess, sys, urllib.request
open("test_demo.py", "w").write(
    "import unittest\n"
    "class T(unittest.TestCase):\n"
    "    def test_ok(self):\n"
    "        self.assertEqual(1 + 1, 2)\n"
)
r = subprocess.run([sys.executable, "-m", "unittest", "test_demo"], capture_output=True, text=True)
open("test_ok.txt", "w").write("pass" if r.returncode == 0 else "fail")
try:
    urllib.request.urlopen("http://example.com", timeout=4)
    open("net.txt", "w").write("net:open")
except Exception:
    open("net.txt", "w").write("net:blocked")
open("host.txt", "w").write(socket.gethostname())
PYEOF`

// continueScript proves workspace continuity (round two reads round
// one's product) and container freshness (a new hostname) in one call.
const continueScript = `cat out.txt > round2.txt && hostname > host2.txt`

// longScript outlives a mid-flight backend options change: after the
// sleep the sandbox reports which image it was created from (the marker
// file only exists on the marker image).
const longScript = `sleep 8; if [ -f /marker ]; then echo marker > img.txt; else echo base > img.txt; fi`

// markerScript surfaces the hot-changed image's marker file.
const markerScript = `cat /marker > marker.txt`

// scriptFor resolves the scripted steps for one conversation: a demo
// token picks the scenario, otherwise the legacy sandboxlab script.
func scriptFor(body []byte) []scriptStep {
	s := string(body)
	switch {
	case strings.Contains(s, tokenBuild):
		return []scriptStep{
			{tool: "Write", args: `{"file_path":"out.txt","content":"woca-demo-r1"}`},
			{tool: "Bash", args: bashArgs(buildScript)},
			{text: "sandbox demo round one complete"},
		}
	case strings.Contains(s, tokenContinue):
		return []scriptStep{
			{tool: "Bash", args: bashArgs(continueScript)},
			{text: "sandbox demo round two complete"},
		}
	case strings.Contains(s, tokenLong):
		return []scriptStep{
			{tool: "Bash", args: bashArgs(longScript)},
			{text: "sandbox demo long run complete"},
		}
	case strings.Contains(s, tokenMarker):
		return []scriptStep{
			{tool: "Bash", args: bashArgs(markerScript)},
			{text: "sandbox demo marker round complete"},
		}
	}
	// Legacy sandboxlab script (default behavior, unchanged).
	return []scriptStep{
		{tool: "Write", args: `{"file_path":"out.txt","content":"sandbox-side"}`},
		{text: "sandbox run complete"},
	}
}

// bashArgs packs one shell command into the Bash tool's argument JSON.
func bashArgs(command string) string {
	b, _ := json.Marshal(map[string]string{"command": command})
	return string(b)
}

// countToolResults counts accumulated tool_result blocks in the request:
// the turn discriminator (turn N of a script sees N-1 results).
func countToolResults(body []byte) int {
	var req struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return 0
	}
	n := 0
	for _, m := range req.Messages {
		var blocks []struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(m.Content, &blocks); err != nil {
			continue // string content, not a block array
		}
		for _, b := range blocks {
			if b.Type == "tool_result" {
				n++
			}
		}
	}
	return n
}

func serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	steps := scriptFor(body)
	// The turn index: after N tool results the conversation is in turn
	// N+1. Past the script's end keep answering plain text so an
	// over-long loop still terminates.
	step := steps[min(countToolResults(body), len(steps)-1)]

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	emit := func(event string, v any) {
		data, _ := json.Marshal(v)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
		if flusher != nil {
			flusher.Flush()
		}
	}
	emit("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": "msg_fakemodel", "type": "message", "role": "assistant",
			"model": "fake-claude", "content": []any{},
			"usage": map[string]int{"input_tokens": 16, "output_tokens": 16},
		},
	})
	if step.tool != "" {
		// One tool call as the turn's answer.
		emit("content_block_start", map[string]any{
			"type": "content_block_start", "index": 0,
			"content_block": map[string]any{
				// NOTE: no "input" here — the partner seeds the argument
				// accumulator from content_block_start.input when present,
				// which would corrupt the partial_json stream ({}{...}).
				"type": "tool_use", "id": "toolu_fakemodel", "name": step.tool,
			},
		})
		emit("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": 0,
			"delta": map[string]string{
				"type":         "input_json_delta",
				"partial_json": step.args,
			},
		})
		emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
		emit("message_delta", map[string]any{
			"type":  "message_delta",
			"delta": map[string]any{"stop_reason": "tool_use", "stop_sequence": nil},
			"usage": map[string]int{"output_tokens": 8},
		})
	} else {
		// Final turn: plain text, stop the loop.
		emit("content_block_start", map[string]any{
			"type": "content_block_start", "index": 0,
			"content_block": map[string]any{"type": "text", "text": ""},
		})
		emit("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": 0,
			"delta": map[string]string{"type": "text_delta", "text": step.text},
		})
		emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
		emit("message_delta", map[string]any{
			"type":  "message_delta",
			"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
			"usage": map[string]int{"output_tokens": 8},
		})
	}
	emit("message_stop", map[string]any{"type": "message_stop"})
}
