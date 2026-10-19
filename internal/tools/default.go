package tools

import (
	"context"

	coretools "github.com/projanvil/langchain-golang/core/tools"

	"github.com/projanvil/woca/internal/platform/executor"
)

// defaultTools assembles the built-in tool set, bound to the harness working
// directory and sharing the session todo store. Shell and file side effects
// resolve through execFor — the registry's per-invocation executor resolver
// (run executor from the context when present, the build-time executor
// otherwise). Task-family tools (subagents) are built by the agent package
// because they need the subagent runner.
func defaultTools(d *Dir, todos *TodoStore, bg *BackgroundShells, fh *FileHistory, lsp *lspPool, execFor func(context.Context) executor.Executor) []coretools.Tool {
	return []coretools.Tool{
		bashTool(d, bg, execFor),
		bashOutputTool(bg),
		killShellTool(bg),
		readTool(d, execFor),
		writeTool(d, fh, execFor),
		editTool(d, fh, execFor),
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
