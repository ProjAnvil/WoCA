// fakemodel is the scripted Anthropic-Messages-API stand-in for the
// sandboxlab e2e stack: it answers POST {base}/messages with the SSE
// stream shape the langchain-golang anthropic partner consumes
// (partners/anthropic/stream.go). The script is fixed:
//
//   - a request WITHOUT a tool_result (the first turn) yields one
//     tool_use block calling the Write tool with {"file_path":"out.txt",
//     "content":"sandbox-side"} — the product the test asserts on the
//     shared workspace volume after the worker downloads it back;
//   - a request WITH a tool_result (the turn after the tool ran) yields
//     a plain text block, ending the run.
//
// Single-file, stdlib-only, no woca imports — it builds inside its own
// tiny docker stage (see Dockerfile beside this file). The wire shape is
// pinned by the smoke test below against the real anthropic client.
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

func serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 8<<20))
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
	if strings.Contains(string(body), "tool_result") {
		// Final turn: plain text, stop the loop.
		emit("content_block_start", map[string]any{
			"type": "content_block_start", "index": 0,
			"content_block": map[string]any{"type": "text", "text": ""},
		})
		emit("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": 0,
			"delta": map[string]string{"type": "text_delta", "text": "sandbox run complete"},
		})
		emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
		emit("message_delta", map[string]any{
			"type":  "message_delta",
			"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
			"usage": map[string]int{"output_tokens": 8},
		})
	} else {
		// First turn: one Write tool call producing the run's artifact.
		emit("content_block_start", map[string]any{
			"type": "content_block_start", "index": 0,
			"content_block": map[string]any{
				// NOTE: no "input" here — the partner seeds the argument
				// accumulator from content_block_start.input when present,
				// which would corrupt the partial_json stream ({}{...}).
				"type": "tool_use", "id": "toolu_fakemodel", "name": "Write",
			},
		})
		emit("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": 0,
			"delta": map[string]string{
				"type":         "input_json_delta",
				"partial_json": `{"file_path":"out.txt","content":"sandbox-side"}`,
			},
		})
		emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
		emit("message_delta", map[string]any{
			"type":  "message_delta",
			"delta": map[string]any{"stop_reason": "tool_use", "stop_sequence": nil},
			"usage": map[string]int{"output_tokens": 8},
		})
	}
	emit("message_stop", map[string]any{"type": "message_stop"})
}
