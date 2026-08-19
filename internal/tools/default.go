package tools

import (
	coretools "github.com/projanvil/langchain-golang/core/tools"

	"github.com/projanvil/woca/internal/platform/executor"
)

// defaultTools assembles the built-in tool set, bound to the harness working
// directory and sharing the session todo store. Shell and file side effects
// go through x. Task-family tools (subagents) are built by the agent package
// because they need the subagent runner.
func defaultTools(d *Dir, todos *TodoStore, bg *BackgroundShells, fh *FileHistory, lsp *lspPool, x executor.Executor) []coretools.Tool {
	return []coretools.Tool{
		bashTool(d, bg, x),
		bashOutputTool(bg),
		killShellTool(bg),
		readTool(d, x),
		writeTool(d, fh, x),
		editTool(d, fh, x),
		notebookEditTool(d, fh),
		globTool(d),
		grepTool(d),
		listDirTool(d),
		lspTool(d, lsp),
		todoWriteTool(todos),
		webFetchTool(),
		webSearchTool(),
		AskUserQuestionTool(),
		EnterPlanModeTool(),
		ExitPlanModeTool(),
	}
}
