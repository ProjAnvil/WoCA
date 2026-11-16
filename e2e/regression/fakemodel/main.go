// fakemodel is the regression runner's scripted Anthropic-Messages-API
// stand-in: same wire shape as e2e/sandboxlab/fakemodel (pinned there
// against the real partner), with the regression scenarios keyed by
// marker tokens riding the first user message:
//
//   - "woca-reg:chat"  turn 1 Write out.txt (the approval round the
//                      stage answers through the HITL API), turn 2 the
//                      final text;
//   - "woca-reg:mcp"   turn 1 one call to the MCP stub tool
//                      mcp__mcpstub__echo (proves MCP binding by
//                      executing it), turn 2 the final text;
//   - anything else    plain text reply (a no-tool chat).
//
// Single-file, stdlib-only, no woca imports.
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
	addr := flag.String("addr", ":13885", "listen address")
	flag.Parse()

	fmt.Fprintf(os.Stderr, "regression fakemodel: %s\n", *addr)
	if err := http.ListenAndServe(*addr, newHandler()); err != nil {
		fmt.Fprintf(os.Stderr, "regression fakemodel: %v\n", err)
		os.Exit(1)
	}
}

func newHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "ok") })
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
	args string
	text string
}

const (
	tokenChat = "woca-reg:chat"
	tokenMCP  = "woca-reg:mcp"
)

func scriptFor(body []byte) []scriptStep {
	// Route on the FIRST user message only: the raw body also carries
	// tool results and injected memory episodes, whose recorded text can
	// contain earlier markers and would misroute the script.
	s := firstUserText(body)
	switch {
	case strings.Contains(s, tokenChat):
		return []scriptStep{
			{tool: "Write", args: `{"file_path":"out.txt","content":"woca-reg-chat-side"}`},
			{text: "woca-reg:chat complete"},
		}
	case strings.Contains(s, tokenMCP):
		return []scriptStep{
			{tool: "mcp__mcpstub__echo", args: `{"text":"reg-mcp-ping"}`},
			{text: "woca-reg:mcp complete"},
		}
	}
	return []scriptStep{{text: "woca-reg:plain reply"}}
}

// firstUserText extracts the first message with role "user": its plain
// string content, or the concatenated text blocks when the partner sends
// content as a block array. Missing/odd shapes fall back to the whole body.
func firstUserText(body []byte) string {
	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return string(body)
	}
	for _, m := range req.Messages {
		if m.Role != "user" {
			continue
		}
		var s string
		if err := json.Unmarshal(m.Content, &s); err == nil && s != "" {
			return s
		}
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(m.Content, &blocks); err == nil {
			var sb strings.Builder
			for _, b := range blocks {
				if b.Type == "text" {
					sb.WriteString(b.Text)
				}
			}
			if sb.Len() > 0 {
				return sb.String()
			}
		}
	}
	return string(body)
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
	// One line per request: the routing text and the tool-result count —
	// enough to debug script misrouting without dumping conversations.
	fmt.Fprintf(os.Stderr, "fakemodel: results=%d first-user=%q\n", countToolResults(body), firstUserText(body))
	steps := scriptFor(body)
	// Past the script's end keep answering plain text so an over-long
	// loop still terminates.
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
			"id": "msg_regfake", "type": "message", "role": "assistant",
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
				"type": "tool_use", "id": "toolu_regfake", "name": step.tool,
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
