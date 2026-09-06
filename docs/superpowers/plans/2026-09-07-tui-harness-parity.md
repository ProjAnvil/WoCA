# TUI Harness Parity Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make Woca's TUI support file-context mentions, a practical composer,
keybinding customization, and discoverable tools/hooks.

**Architecture:** `internal/promptref` owns safe workspace-bound `@path`
expansion and completion. `internal/keybindings` owns persisted action maps.
The TUI consumes both packages while `/tools`, `/hooks`, and `/keybindings`
remain in the shared console layer.

**Tech Stack:** Go 1.26, Bubble Tea, Cobra, existing Woca console and agent
packages.

**Spec:** `docs/superpowers/specs/2026-09-07-tui-harness-parity-design.md`

## Global Constraints

- Preserve `/skill` and `@agent` behavior.
- Expand only regular text files inside the active workspace.
- Keep the original author text in transcript and session history display.
- Default keys retain existing behavior.
- Do not add dependencies.

---

### Task 1: Safe prompt-reference package

**Files:**
- Create: `internal/promptref/promptref.go`
- Create: `internal/promptref/promptref_test.go`
- Modify: `internal/tui/complete.go`
- Modify: `internal/tui/tui.go`
- Test: `internal/tui/keys_test.go`

**Interfaces:**
- Produces `promptref.Expand(text, workdir string) (string, error)`.
- Produces `promptref.Candidates(prefix, workdir string) []Candidate`.
- `Candidate` has `Path string` and `IsDir bool`.

- [x] **Step 1: Write failing reference tests**

```go
func TestExpandIncludesWorkspaceFile(t *testing.T) {
    workspace := t.TempDir()
    os.WriteFile(filepath.Join(workspace, "note.txt"), []byte("hello"), 0o644)
    got, err := Expand("read @note.txt", workspace)
    if err != nil || !strings.Contains(got, "<woca-file path=\"note.txt\">") || !strings.Contains(got, "hello") {
        t.Fatalf("got %q, err %v", got, err)
    }
}
```

- [x] **Step 2: Run the focused test and verify it fails because `Expand` is missing**

Run: `go test ./internal/promptref -run TestExpandIncludesWorkspaceFile -count=1`

- [x] **Step 3: Implement reference parsing and workspace confinement**

```go
func Expand(text, workdir string) (string, error) {
    // Parse trailing-token-safe @path references, reject escaping paths, and
    // replace each reference with a delimited file-context block.
}
```

- [x] **Step 4: Add tests for directory bounds, missing paths, and `@agent` preservation**

```go
func TestExpandLeavesNonPathMentionUntouched(t *testing.T) {
    got, err := Expand("delegate to @reviewer", t.TempDir())
    if err != nil || got != "delegate to @reviewer" { t.Fatalf("%q %v", got, err) }
}
```

- [x] **Step 5: Wire expansion and candidates into the TUI, then run tests**

Run: `go test ./internal/promptref ./internal/tui -run 'Test(Expand|At)' -count=1`

### Task 2: Multiline and external editor composer

**Files:**
- Modify: `internal/tui/tui.go`
- Modify: `internal/tui/keys_test.go`
- Modify: `internal/tui/tui_test.go`

**Interfaces:**
- Produces `(*Model).insertNewline()` and editor-return message handling.
- Consumes expanded prompt text from Task 1.

- [x] **Step 1: Write failing TUI key tests**

```go
func TestCtrlJInsertsNewlineWithoutDispatch(t *testing.T) {
    model := New(nil, nil, config.Config{})
    model.input = "first"
    model.cursor = len(model.input)
    model = press(model, ctrlKey("ctrl+j"))
    if model.input != "first\n" || model.running { t.Fatalf("%q", model.input) }
}
```

- [x] **Step 2: Run the focused test and verify it fails**

Run: `go test ./internal/tui -run TestCtrlJInsertsNewlineWithoutDispatch -count=1`

- [x] **Step 3: Implement multiline insertion and visual composer sizing**

```go
case "ctrl+j":
    m.input = m.input[:m.cursor] + "\n" + m.input[m.cursor:]
    m.cursor++
```

- [x] **Step 4: Add editor-draft recovery tests, then implement `$VISUAL`/`$EDITOR` execution**

```go
func TestEditorResultReplacesDraft(t *testing.T) {
    model := New(nil, nil, config.Config{})
    model.input = "old"
    model.handleEditorResult(editorResult{content: "new\ntext"})
    if model.input != "new\ntext" { t.Fatal(model.input) }
}
```

- [x] **Step 5: Run focused composer tests**

Run: `go test ./internal/tui -run 'Test(CtrlJ|Editor|ViewContainsPromptBox)' -count=1`

### Task 3: Keybinding configuration

**Files:**
- Create: `internal/keybindings/keybindings.go`
- Create: `internal/keybindings/keybindings_test.go`
- Modify: `internal/tui/tui.go`
- Modify: `internal/console/uicmd.go`
- Modify: `internal/console/console.go`
- Modify: `internal/slashcmds/slashcmds.go`
- Test: `internal/console/uicmd_test.go`

**Interfaces:**
- Produces `keybindings.Load(path string) (Map, error)` and `Map.Action(key string) string`.
- Consumes the map in `tui.Model.Update` before default key handling.

- [x] **Step 1: Write failing default and override tests**

```go
func TestOverrideReplacesDefaultAction(t *testing.T) {
    m := Default()
    m.Apply(map[string]string{"ctrl+p": "composer:newline"})
    if got := m.Action("ctrl+p"); got != "composer:newline" { t.Fatal(got) }
}
```

- [x] **Step 2: Run the focused test and verify it fails**

Run: `go test ./internal/keybindings -run TestOverrideReplacesDefaultAction -count=1`

- [x] **Step 3: Implement strict JSON validation and default action lookup**

```go
type Map struct { bindings map[string]string }
func (m Map) Action(key string) string { return m.bindings[normalize(key)] }
```

- [x] **Step 4: Add `/keybindings` effective-map and malformed-config tests**

```go
func TestKeybindingsCommandShowsCtrlJ(t *testing.T) {
    res := newConsole(t).keybindingsCmd(nil)
    if !strings.Contains(res.Output, "ctrl+j") { t.Fatal(res.Output) }
}
```

- [x] **Step 5: Run keybinding package, console, and TUI tests**

Run: `go test ./internal/keybindings ./internal/console ./internal/tui -count=1`

### Task 4: Tool and hook discovery commands

**Files:**
- Modify: `internal/console/console.go`
- Modify: `internal/console/console_test.go`
- Modify: `internal/slashcmds/slashcmds.go`
- Modify: `README.md`
- Modify: `README.zh-CN.md`

**Interfaces:**
- Produces `/tools` and `/hooks` command output through `Console.Execute`.
- Consumes the configured agent registry and hook runner.

- [x] **Step 1: Write failing command tests**

```go
func TestToolsCommandListsRegisteredTool(t *testing.T) {
    result, err := console.Execute(t.Context(), "tools", nil, &Session{})
    if err != nil || !strings.Contains(result.Output, "Read") { t.Fatal(result, err) }
}
```

- [x] **Step 2: Run focused tests and verify `/tools` is unknown**

Run: `go test ./internal/console -run TestToolsCommandListsRegisteredTool -count=1`

- [x] **Step 3: Implement console output and add registry entries**

```go
case "tools": return Result{Output: c.tools()}, nil
case "hooks": return Result{Output: c.hooks()}, nil
```

- [x] **Step 4: Document command parity and run focused tests**

Run: `go test ./internal/console ./internal/slashcmds -count=1`

### Task 5: Integration verification

**Files:**
- Modify: `README.md`
- Modify: `README.zh-CN.md`

- [x] **Step 1: Update TUI documentation with `@path`, multiline, editor, keybinding, `/tools`, and `/hooks` usage**
- [x] **Step 2: Run formatting**

Run: `gofmt -w internal/promptref/*.go internal/keybindings/*.go internal/tui/*.go internal/console/*.go internal/slashcmds/*.go`

- [x] **Step 3: Run the full Go suite**

Run: `go test ./...`

- [x] **Step 4: Inspect the diff and report unaddressed parity gaps**

Run: `git diff --check && git diff --stat`
