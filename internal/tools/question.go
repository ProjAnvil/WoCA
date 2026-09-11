package tools

import (
	"context"
	"fmt"
	"strings"

	coretools "github.com/projanvil/langchain-golang/core/tools"
)

// QuestionOption is one selectable answer in an AskUserQuestion question.
type QuestionOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// Question is a single multiple-choice question surfaced to the user.
type Question struct {
	Question    string           `json:"question"`
	Header      string           `json:"header,omitempty"`
	Options     []QuestionOption `json:"options"`
	MultiSelect bool             `json:"multi_select,omitempty"`
}

const askUserQuestionDescription = `Ask the user one or more multiple-choice questions to gather information, clarify ambiguity, or offer choices. Use only when genuinely needed.`

type askUserQuestionArgs struct {
	Questions []Question `json:"questions"`
}

// AskUserQuestionTool is registered so the model can request user input. Its
// actual execution is intercepted by the agent's interaction gate (which
// pauses the graph via interrupt), so the FromFunc body here is a fallback.
func AskUserQuestionTool() coretools.Tool {
	t := MustTool(coretools.FromFunc("AskUserQuestion", askUserQuestionDescription,
		func(ctx context.Context, a askUserQuestionArgs) (coretools.Result, error) {
			if len(a.Questions) == 0 {
				return coretools.Result{}, fmt.Errorf("at least one question is required")
			}
			return coretools.Result{Content: "AskUserQuestion is handled by the harness."}, nil
		}))
	return t
}

const enterPlanModeDescription = `Request to enter plan mode: research the codebase, write an implementation plan, and get approval before writing code. Use for complex, multi-step implementation tasks.`

type enterPlanModeArgs struct{}

// EnterPlanModeTool is registered so the model can request plan mode. The
// agent's interaction gate intercepts it and toggles plan mode.
func EnterPlanModeTool() coretools.Tool {
	t := MustTool(coretools.FromFunc("EnterPlanMode", enterPlanModeDescription,
		func(ctx context.Context, a enterPlanModeArgs) (coretools.Result, error) {
			return coretools.Result{Content: "EnterPlanMode is handled by the harness."}, nil
		}))
	return t
}

const exitPlanModeDescription = `Signal that planning is complete and request user approval to proceed with implementation. Use only when in plan mode.`

type exitPlanModeArgs struct {
	Plan string `json:"plan,omitempty"`
}

// ExitPlanModeTool is registered so the model can request plan approval. The
// agent's interaction gate intercepts it, surfaces the plan, and on approval
// exits plan mode.
func ExitPlanModeTool() coretools.Tool {
	t := MustTool(coretools.FromFunc("ExitPlanMode", exitPlanModeDescription,
		func(ctx context.Context, a exitPlanModeArgs) (coretools.Result, error) {
			return coretools.Result{Content: "ExitPlanMode is handled by the harness."}, nil
		}))
	return t
}

// FormatQuestion formats a single question for terminal display.
func FormatQuestion(i int, q Question) string {
	var b strings.Builder
	if q.Header != "" {
		fmt.Fprintf(&b, "%s\n", q.Header)
	}
	fmt.Fprintf(&b, "%d. %s\n", i+1, q.Question)
	for j, opt := range q.Options {
		label := opt.Label
		if opt.Description != "" {
			label = fmt.Sprintf("%s — %s", label, opt.Description)
		}
		fmt.Fprintf(&b, "   %d) %s\n", j+1, label)
	}
	if q.MultiSelect {
		fmt.Fprintf(&b, "   (select multiple, comma-separated)\n")
	}
	return b.String()
}
