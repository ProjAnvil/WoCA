package console

import (
	"os"
	"path/filepath"
)

// initPrompt is submitted as a user turn by /init: the agent explores the
// repo with its own tools and writes the project guide file itself.
const initPrompt = `Please analyze this codebase and create a project guide file with:

1. Commands that will be commonly used, such as how to build, lint, and run tests. Include the necessary flags.
2. High-level code architecture and structure so new engineers can quickly become productive: the main packages/modules, how they relate, and where key behavior lives.
3. Any weirdness, quirks, gotchas, or anything else an engineer touching this repo should know.

For each item include file paths and short code snippets showing common patterns. Read the existing docs (README, CONTRIBUTING, existing guides) first and follow the conventions and style you find. Be concrete — no filler or generic advice.

After a thorough analysis, use the Write tool to create the guide file. Keep it concise and skimmable.`

// initCmd implements /init: choose the guide filename (update CLAUDE.md when
// one exists, otherwise create AGENTS.md) and return the analysis prompt for
// the driver to submit as a user turn.
func (c *Console) initCmd(session *Session) (Result, error) {
	name := "AGENTS.md"
	if _, err := os.Stat(filepath.Join(c.deps.Cfg.WorkDir, "CLAUDE.md")); err == nil {
		name = "CLAUDE.md"
	}
	return Result{
		Output: "Analyzing the codebase to write " + name + " …\n",
		Prompt: initPrompt + "\n\nWrite the guide to " + name + " in the repository root.",
	}, nil
}
