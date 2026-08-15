package tools

import coretools "github.com/projanvil/langchain-golang/core/tools"

// defaultTools assembles the built-in tool set, bound to the harness working
// directory and sharing the session todo store. Task-family tools (subagents)
// are built by the agent package because they need the subagent runner.
func defaultTools(workdir string, todos *TodoStore) []coretools.Tool {
	return []coretools.Tool{
		bashTool(workdir),
		readTool(workdir),
		writeTool(workdir),
		editTool(workdir),
		notebookEditTool(workdir),
		globTool(workdir),
		grepTool(workdir),
		listDirTool(workdir),
		todoWriteTool(todos),
		webFetchTool(),
		webSearchTool(),
		AskUserQuestionTool(),
		EnterPlanModeTool(),
		ExitPlanModeTool(),
	}
}
