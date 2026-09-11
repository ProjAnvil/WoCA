package tools

import coretools "github.com/projanvil/langchain-golang/core/tools"

// MustTool panics on tool-construction errors; construction failure is a
// programmer error (static definitions), not runtime input.
func MustTool(t coretools.Func, err error) coretools.Func {
	if err != nil {
		panic(err)
	}
	return t
}
