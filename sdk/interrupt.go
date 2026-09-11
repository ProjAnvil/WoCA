package sdk

import (
	"fmt"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/runner"
)

// Interrupt is the public view of a graph pause: exactly one field is set.
type Interrupt struct {
	Kind       string // permission | ask_user | plan_approval
	Permission *PermissionInterrupt
	AskUser    *AskUserInterrupt
	Plan       *PlanInterrupt
}

type PermissionInterrupt struct {
	ToolName   string
	ToolArgs   map[string]any
	Reason     string
	IsReadOnly bool
}

type AskUserInterrupt struct {
	Questions []string // question texts; answers are []string in question order
}

type PlanInterrupt struct {
	Plan string
}

// AskUserAnswer is the resume value for an ask_user interrupt: one answer
// per question, in question order.
type AskUserAnswer struct{ Answers []string }

// interruptHandler returns the runner handler: the caller's OnInterrupt when
// set, else the headless default matching the CLI's non-interactive posture
// (deny permissions, empty answers, reject plans).
func (o Options) interruptHandler() runner.InterruptHandler {
	return func(value any) (any, error) {
		if o.OnInterrupt != nil {
			v, err := o.OnInterrupt(interruptFrom(value))
			if err != nil {
				return nil, err
			}
			if ans, ok := v.(AskUserAnswer); ok {
				return agent.AskUserAnswer{Answers: ans.Answers}, nil
			}
			return v, nil
		}
		switch value.(type) {
		case agent.PermissionRequest:
			return "n", nil
		case agent.AskUserRequest:
			return agent.AskUserAnswer{}, nil
		case agent.PlanApprovalRequest:
			return "n", nil
		default:
			return nil, fmt.Errorf("sdk: unknown interrupt value %T", value)
		}
	}
}

func interruptFrom(value any) Interrupt {
	switch v := value.(type) {
	case agent.PermissionRequest:
		return Interrupt{Kind: "permission", Permission: &PermissionInterrupt{
			ToolName: v.ToolName, ToolArgs: v.ToolArgs, Reason: v.Reason, IsReadOnly: v.IsReadOnly,
		}}
	case agent.AskUserRequest:
		qs := make([]string, 0, len(v.Questions))
		for _, q := range v.Questions {
			qs = append(qs, q.Question)
		}
		return Interrupt{Kind: "ask_user", AskUser: &AskUserInterrupt{Questions: qs}}
	case agent.PlanApprovalRequest:
		return Interrupt{Kind: "plan_approval", Plan: &PlanInterrupt{Plan: v.Plan}}
	default:
		return Interrupt{Kind: fmt.Sprintf("unknown:%T", value)}
	}
}
