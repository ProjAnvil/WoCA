package tools

import coretools "github.com/projanvil/langchain-golang/core/tools"

// defaultTools assembles the built-in tool set, bound to the harness working
// directory and sharing the session todo store. Task-family tools (subagents)
// are built by the agent package because they need the subagent runner.
func defaultTools(d *Dir, todos *TodoStore, bg *BackgroundShells, fh *FileHistory, lsp *lspPool) []coretools.Tool {
	return []coretools.Tool{
		bashTool(d, bg),
		bashOutputTool(bg),
		killShellTool(bg),
		readTool(d),
		writeTool(d, fh),
		editTool(d, fh),
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
