package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	coretools "github.com/projanvil/langchain-golang/core/tools"
)

const notebookEditDescription = `Edit a Jupyter notebook (.ipynb) cell's source. Identify the cell by cell_id or cell_index (0-based).`

type notebookEditArgs struct {
	NotebookPath string `json:"notebook_path"`
	CellID       string `json:"cell_id,omitempty"`
	CellIndex    int    `json:"cell_index,omitempty"`
	NewSource    string `json:"new_source"`
}

func notebookEditTool(baseWorkdir string) coretools.Tool {
	t, err := coretools.FromFunc("NotebookEdit", notebookEditDescription,
		func(ctx context.Context, a notebookEditArgs) (coretools.Result, error) {
			path := resolvePath(baseWorkdir, a.NotebookPath)
			data, err := os.ReadFile(path)
			if err != nil {
				return coretools.Result{}, fmt.Errorf("notebook %s: %w", a.NotebookPath, err)
			}
			var nb struct {
				Cells []struct {
					ID     string `json:"id"`
					Source any    `json:"source"`
					Type   string `json:"cell_type"`
				} `json:"cells"`
			}
			if err := json.Unmarshal(data, &nb); err != nil {
				return coretools.Result{}, fmt.Errorf("parse notebook %s: %w", a.NotebookPath, err)
			}

			idx := -1
			if a.CellID != "" {
				for i, c := range nb.Cells {
					if c.ID == a.CellID {
						idx = i
						break
					}
				}
				if idx == -1 {
					return coretools.Result{}, fmt.Errorf("cell_id %q not found", a.CellID)
				}
			} else {
				idx = a.CellIndex
				if idx < 0 || idx >= len(nb.Cells) {
					return coretools.Result{}, fmt.Errorf("cell_index %d out of range (0-%d)", a.CellIndex, len(nb.Cells)-1)
				}
			}

			nb.Cells[idx].Source = splitLines(a.NewSource)
			out, err := json.MarshalIndent(nb, "", "  ")
			if err != nil {
				return coretools.Result{}, err
			}
			if err := os.WriteFile(path, out, 0o644); err != nil {
				return coretools.Result{}, fmt.Errorf("write notebook %s: %w", a.NotebookPath, err)
			}
			return coretools.Result{Content: fmt.Sprintf("Edited cell %d in %s", idx, a.NotebookPath)}, nil
		})
	if err != nil {
		panic(err)
	}
	return t
}

// splitLines converts a string into the ipynb "source" representation: a list
// of lines (each newline-terminated) so round-trips preserve line structure.
func splitLines(s string) []string {
	if s == "" {
		return []string{}
	}
	lines := strings.SplitAfter(s, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}
