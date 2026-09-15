package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeNotebook(t *testing.T, path string) {
	t.Helper()
	nb := `{
 "cells": [
  {"id": "alpha", "cell_type": "code", "source": ["print(1)\n"]},
  {"id": "beta", "cell_type": "markdown", "source": ["# hi\n"]}
 ]
}`
	if err := os.WriteFile(path, []byte(nb), 0o644); err != nil {
		t.Fatal(err)
	}
}

func notebookSources(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var nb struct {
		Cells []struct {
			ID     string `json:"id"`
			Source any    `json:"source"`
		} `json:"cells"`
	}
	if err := json.Unmarshal(data, &nb); err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(nb.Cells))
	for _, c := range nb.Cells {
		b, _ := json.Marshal(c.Source)
		out = append(out, string(b))
	}
	return out
}

func TestNotebookEditByCellID(t *testing.T) {
	dir := t.TempDir()
	reg := NewRegistry(dir)
	tool, ok := reg.Get("NotebookEdit")
	if !ok {
		t.Fatal("NotebookEdit tool missing")
	}
	nbPath := filepath.Join(dir, "nb.ipynb")
	writeNotebook(t, nbPath)

	res, err := tool.Invoke(t.Context(), map[string]any{
		"notebook_path": "nb.ipynb", "cell_id": "beta", "new_source": "# hello\nworld",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "Edited cell 1") {
		t.Fatalf("NotebookEdit = %q", res.Content)
	}
	srcs := notebookSources(t, nbPath)
	if len(srcs) != 2 {
		t.Fatalf("cells = %d, want 2", len(srcs))
	}
	if srcs[0] != `["print(1)\n"]` || srcs[1] != `["# hello\n","world"]` {
		t.Fatalf("sources after edit = %v", srcs)
	}
	if undo, redo := reg.FileHistory().Counts(t.Context()); undo != 1 || redo != 0 {
		t.Fatalf("history counts = %d/%d, want 1/0", undo, redo)
	}
	if msg, err := reg.FileHistory().Undo(t.Context()); err != nil || !strings.Contains(msg, "nb.ipynb") {
		t.Fatalf("undo after notebook edit = %q, %v", msg, err)
	}
	if srcs := notebookSources(t, nbPath); srcs[1] != `["# hi\n"]` {
		t.Fatalf("undo did not restore the notebook: %v", srcs)
	}
}

func TestNotebookEditByCellIndex(t *testing.T) {
	dir := t.TempDir()
	reg := NewRegistry(dir)
	tool, _ := reg.Get("NotebookEdit")
	nbPath := filepath.Join(dir, "nb.ipynb")
	writeNotebook(t, nbPath)

	res, err := tool.Invoke(t.Context(), map[string]any{
		"notebook_path": "nb.ipynb", "cell_index": 0, "new_source": "x=1\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "Edited cell 0") {
		t.Fatalf("NotebookEdit = %q", res.Content)
	}
	if srcs := notebookSources(t, nbPath); srcs[0] != `["x=1\n"]` {
		t.Fatalf("source[0] = %v", srcs[0])
	}
}

func TestNotebookEditErrors(t *testing.T) {
	dir := t.TempDir()
	reg := NewRegistry(dir)
	tool, _ := reg.Get("NotebookEdit")
	nbPath := filepath.Join(dir, "nb.ipynb")
	writeNotebook(t, nbPath)

	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{
			"missing notebook",
			map[string]any{"notebook_path": "gone.ipynb", "cell_index": 0, "new_source": "x"},
			"notebook gone.ipynb",
		},
		{
			"bad json",
			map[string]any{"notebook_path": "bad.ipynb", "cell_index": 0, "new_source": "x"},
			"parse notebook bad.ipynb",
		},
		{
			"unknown cell_id",
			map[string]any{"notebook_path": "nb.ipynb", "cell_id": "nope", "new_source": "x"},
			`cell_id "nope" not found`,
		},
		{
			"cell_index negative",
			map[string]any{"notebook_path": "nb.ipynb", "cell_index": -1, "new_source": "x"},
			"out of range",
		},
		{
			"cell_index too large",
			map[string]any{"notebook_path": "nb.ipynb", "cell_index": 5, "new_source": "x"},
			"out of range",
		},
	}
	if err := os.WriteFile(filepath.Join(dir, "bad.ipynb"), []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			_, err := tool.Invoke(t.Context(), c.args)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("NotebookEdit error = %v, want %q", err, c.want)
			}
		})
	}
}

func TestNotebookEditWriteFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permissions do not block writes")
	}
	dir := t.TempDir()
	nbPath := filepath.Join(dir, "nb.ipynb")
	writeNotebook(t, nbPath)
	// An existing file stays writable through the directory's mode, so make
	// the notebook itself read-only to force the rewrite to fail.
	if err := os.Chmod(nbPath, 0o444); err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry(dir)
	tool, _ := reg.Get("NotebookEdit")
	if _, err := tool.Invoke(t.Context(), map[string]any{
		"notebook_path": "nb.ipynb", "cell_index": 0, "new_source": "x",
	}); err == nil || !strings.Contains(err.Error(), "write notebook nb.ipynb") {
		t.Fatalf("read-only notebook write = %v", err)
	}
}

func TestSplitLines(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", []string{}},
		{"one", []string{"one"}},
		{"a\nb", []string{"a\n", "b"}},
		{"a\nb\n", []string{"a\n", "b\n"}},
		{"\n", []string{"\n"}},
	}
	for _, c := range cases {
		got := splitLines(c.in)
		if len(got) != len(c.want) {
			t.Errorf("splitLines(%q) = %v, want %v", c.in, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("splitLines(%q)[%d] = %q, want %q", c.in, i, got[i], c.want[i])
			}
		}
	}
}
