# TUI Harness Parity Design

## Goal

Bring Woca's terminal workflow to parity with the high-value interactive
contracts common to Codex CLI, Claude Code, and Gemini CLI without duplicating
capabilities Woca already exposes through skills, agents, and slash commands.

## Evidence and scope

The existing TUI already has streaming output, approvals, a transcript viewer,
command palette, session/model pickers, queued turns, plan mode, Vim input,
skills, subagents, and a shared slash-command registry.  The review of the
official interactive-mode documentation for Claude Code and Gemini CLI found
four material gaps:

1. `@` does not attach local file or directory context to a TUI prompt.
2. The composer cannot form a multiline prompt or open the current draft in an
   external editor.
3. Key bindings are documented but cannot be remapped by the user.
4. Users cannot inspect the effective tool set or configured hooks from the
   shared command surface.

Project-level repeatable prompts are already covered by Woca skills, which are
autoloaded and manually invokable through `/name`; a second custom-command
format would fragment the extension model.  Existing `/agents`, `/skills`,
`/mcp`, `/plugins`, `/resume`, `/rewind`, `/permissions`, `/model`, `/compact`,
and plan/goal commands already cover the corresponding common harness flows.

## Architecture

### Prompt references

Create an `internal/promptref` package.  It recognizes `@path` references only
when the referenced filesystem path exists below the active workspace, so
existing `@agent` completion and agent invocation remain unchanged.  It reads
regular text files and bounded directory trees into clearly delimited context
blocks before dispatching a human message to the model.  Hidden VCS metadata,
binary files, symlinks that escape the workspace, over-large files, and
over-large directory expansions are rejected with actionable errors.  The TUI
continues to render and save the author's unexpanded text; only the model
message receives the expansion.

The same package owns file-reference completion candidates.  TUI completion
merges matching paths with `@agent` entries and labels their type, replacing
only the trailing `@` token when accepted.

### Composer and editor

The TUI composer treats `ctrl+j` as an inserted newline and `enter` as submit.
The renderer preserves line breaks, cursor position, and prompt-box height.
`ctrl+g` opens the current draft in `$VISUAL`, then `$EDITOR`; if neither is
set it reports a non-destructive hint.  The editor writes to a temporary file;
on return Woca replaces the draft with that file's contents and restores the
cursor.  Editor failures retain the original draft.

### Configurable keys

Add a small `internal/keybindings` package that loads
`~/.woca/keybindings.json`, validates known action names, and resolves a key
to the default action when no override exists.  The TUI uses it for global
actions and composer actions, while overlays retain their modal navigation.
`/keybindings` reports the effective mapping and `/keybindings open` creates
or opens the JSON configuration file.  The default mapping retains existing
behavior, adding `ctrl+j` for newline and `ctrl+g` for the external editor.

### Commands

Add `/tools` and `/hooks` to the shared slash registry and console dispatch.
`/tools` lists every effective agent tool with category and permission safety;
`/hooks` lists event groups from the active hook runner or clearly states that
none are configured.  These commands work identically in TUI, REPL, and web
completion.

## Error handling

- An invalid, unreadable, or out-of-workspace `@` reference aborts submission
  without clearing the draft and surfaces a concise error in the transcript.
- File expansion has deterministic per-file, file-count, and total-content
  limits.  A limit error identifies the offending path.
- Missing editor configuration and unsuccessful editor execution leave the
  draft untouched.
- Invalid keybindings are ignored as a whole, retain defaults, and are
  reported by `/keybindings` so a malformed user config never makes the TUI
  unusable.

## Verification

Package tests prove reference parsing, workspace confinement, bounded directory
expansion, completions, multiline submission, external-editor draft recovery,
keybinding defaults/overrides/validation, and shared-command output.  TUI tests
exercise the corresponding key events.  Finish with `go test ./internal/tui
./internal/console ./internal/promptref ./internal/keybindings` followed by
`go test ./...`.
