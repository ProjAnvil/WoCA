// mcpstub is the regression runner's minimal stdio MCP server: it speaks
// just enough JSON-RPC (initialize / tools/list / tools/call / ping) to
// prove the agent's MCP integration end to end — the agent binds its
// tools as mcp__mcpstub__echo and a scripted model turn executes one.
//
// Single-file, stdlib-only. Newline-delimited JSON-RPC over stdio (the
// MCP stdio transport).
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
)

func main() {
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	w := bufio.NewWriter(os.Stdout)
	defer func() { _ = w.Flush() }()

	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var msg struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(line, &msg); err != nil {
			continue
		}
		if len(msg.ID) == 0 {
			continue // a notification: nothing to answer
		}
		result, err := handle(msg.Method, msg.Params)
		if err != nil {
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":%q}}`+"\n", msg.ID, err.Error())
		} else {
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":%s}`+"\n", msg.ID, result)
		}
		_ = w.Flush()
	}
}

func handle(method string, params json.RawMessage) (string, error) {
	switch method {
	case "initialize":
		return `{"protocolVersion":"2024-11-05","capabilities":{"tools":{}},"serverInfo":{"name":"mcpstub","version":"1.0.0"}}`, nil
	case "ping":
		return `{}`, nil
	case "tools/list":
		return `{"tools":[{"name":"echo","description":"Echo the given text back prefixed with echo:.","inputSchema":{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}}]}`, nil
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(params, &p); err != nil || p.Name != "echo" {
			return `{}`, fmt.Errorf("unknown tool")
		}
		text, _ := p.Arguments["text"].(string)
		return fmt.Sprintf(`{"content":[{"type":"text","text":%q}],"isError":false}`, "echo:"+text), nil
	}
	return `{}`, fmt.Errorf("method not found: %s", method)
}
