// Package agents loads custom subagent definitions from markdown files with
// optional YAML frontmatter, mirroring the skills package's loading style.
package agents

import (
	"cmp"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Definition is a custom subagent definition loaded from a markdown file.
type Definition struct {
	Name        string
	Description string
	Prompt      string   // markdown body used as the subagent's system prompt
	Tools       []string // allowed tool names; empty = inherit the parent tool set
	Source      string   // file the definition was loaded from ("" for inline)
}

// Load walks root and loads every *.md file. Each file has optional YAML-style
// frontmatter between leading "---" lines with keys: name, description, tools
// (comma-separated). The remaining body is the prompt. The default name is the
// filename without extension.
func Load(root string) ([]Definition, error) {
	var out []Definition
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == root {
				return err // surface a missing/unreadable root
			}
			return nil // skip unreadable nested entries
		}
		if d.IsDir() {
			return nil
		}
		if filepath.Ext(d.Name()) != ".md" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		defaultName := strings.TrimSuffix(d.Name(), filepath.Ext(d.Name()))
		def := parseDefinition(defaultName, data)
		def.Source = path
		out = append(out, def)
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(out, func(a, b Definition) int { return cmp.Compare(a.Name, b.Name) })
	return out, nil
}

// Find returns the definition with the given name.
func Find(list []Definition, name string) (Definition, bool) {
	for _, d := range list {
		if d.Name == name {
			return d, true
		}
	}
	return Definition{}, false
}

// Render encodes a definition back into its markdown file format (inverse of
// parseDefinition; used by the /agents manager).
func Render(d Definition) string {
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "name: %s\n", d.Name)
	if d.Description != "" {
		fmt.Fprintf(&b, "description: %s\n", d.Description)
	}
	if len(d.Tools) > 0 {
		fmt.Fprintf(&b, "tools: %s\n", strings.Join(d.Tools, ", "))
	}
	b.WriteString("---\n\n")
	b.WriteString(d.Prompt)
	b.WriteString("\n")
	return b.String()
}

// parseDefinition parses a markdown file with optional YAML frontmatter
// delimited by leading "---" lines.
func parseDefinition(defaultName string, data []byte) Definition {
	text := strings.TrimSpace(string(data))
	d := Definition{Name: defaultName}

	if strings.HasPrefix(text, "---") {
		parts := strings.SplitN(text, "---", 3)
		if len(parts) == 3 {
			for line := range strings.SplitSeq(parts[1], "\n") {
				k, v, ok := strings.Cut(line, ":")
				if !ok {
					continue
				}
				switch strings.TrimSpace(k) {
				case "name":
					d.Name = strings.TrimSpace(v)
				case "description":
					d.Description = strings.TrimSpace(v)
				case "tools":
					d.Tools = splitTools(v)
				}
			}
			d.Prompt = strings.TrimSpace(parts[2])
		} else {
			d.Prompt = text
		}
	} else {
		d.Prompt = text
	}
	return d
}

// splitTools splits a comma-separated tool list, trimming and dropping empty
// entries.
func splitTools(v string) []string {
	var out []string
	for s := range strings.SplitSeq(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}
