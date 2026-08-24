package bridge

// The managed-inventory API: the read-only view of what the config plane
// delivered. Operator skills & MCP materialize as the managed root's
// files (applyManaged); GET /api/managed rebuilds the inventory from
// exactly those files, so the settings page shows what the agent actually
// merges — not a second copy of config state that could drift. Display
// metadata only: env values never cross the wire (the claudeenv ruling).

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/projanvil/woca/internal/skills"
)

// ManagedSkill is one operator-delivered skill.
type ManagedSkill struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// ManagedMCPServer is one operator-delivered MCP server, display form.
type ManagedMCPServer struct {
	Name    string `json:"name"`
	Type    string `json:"type"` // http | stdio
	URL     string `json:"url,omitempty"`
	Command string `json:"command,omitempty"`
}

// ManagedInventory is the GET /api/managed payload.
type ManagedInventory struct {
	Skills []ManagedSkill     `json:"skills"`
	MCP    []ManagedMCPServer `json:"mcp"`
}

// ReadManagedInventory lists the materialized delivery set (empty lists
// when nothing was delivered or the root is unreadable — an unenrolled
// bridge simply has nothing to show).
func ReadManagedInventory() ManagedInventory {
	inv := ManagedInventory{Skills: []ManagedSkill{}, MCP: []ManagedMCPServer{}}
	root := managedRootPath()
	if root == "" {
		return inv
	}
	if list, err := skills.Load(filepath.Join(root, "skills")); err == nil {
		for _, sk := range list {
			inv.Skills = append(inv.Skills, ManagedSkill{Name: sk.Name, Description: sk.Description})
		}
	}
	if servers := readManagedMCP(root); servers != nil {
		inv.MCP = servers
	}
	return inv
}

// readManagedMCP parses the managed root's mcp.json — the same
// {"mcpServers":{...}} carrier materializeManagedMCP writes. Unknown
// shapes are skipped, not fatal: the file is operator-driven and a bad
// row must not hide the rest.
func readManagedMCP(root string) []ManagedMCPServer {
	data, err := os.ReadFile(filepath.Join(root, "mcp.json"))
	if err != nil {
		return nil
	}
	var doc struct {
		Servers map[string]json.RawMessage `json:"mcpServers"`
	}
	if json.Unmarshal(data, &doc) != nil || doc.Servers == nil {
		return nil
	}
	out := make([]ManagedMCPServer, 0, len(doc.Servers))
	for name, raw := range doc.Servers {
		var s struct {
			Type    string   `json:"type"`
			URL     string   `json:"url"`
			Command string   `json:"command"`
			Args    []string `json:"args"`
		}
		if json.Unmarshal(raw, &s) != nil {
			continue
		}
		entry := ManagedMCPServer{Name: name, Type: s.Type, URL: s.URL, Command: s.Command}
		if len(s.Args) > 0 {
			if entry.Command != "" {
				entry.Command += " "
			}
			entry.Command += strings.Join(s.Args, " ")
		}
		// Nothing displayable = unknown shape; skip the row.
		if entry.URL == "" && entry.Command == "" {
			continue
		}
		if entry.Type == "" && entry.URL != "" {
			entry.Type = "http"
		} else if entry.Type == "" && entry.Command != "" {
			entry.Type = "stdio"
		}
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ManagedInventoryRoutes mounts GET /api/managed — the 远端配置 page.
// wrap carries the caller's scope gate (settings:read, the same class as
// the claudeenv page) and is nil on local-trust deployments.
func ManagedInventoryRoutes(mux *http.ServeMux, wrap func(http.HandlerFunc) http.Handler) {
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSONBridge(w, http.StatusOK, ReadManagedInventory())
	})
	if wrap != nil {
		mux.Handle("GET /api/managed", wrap(h))
		return
	}
	mux.HandleFunc("GET /api/managed", h)
}
