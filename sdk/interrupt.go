package sdk

import (
	"fmt"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/runner"
)

// Interrupt is the public view of a graph pause: Kind selects the payload.
// "subagent_permission" sets Subagent plus the inner Permission;
// "approvals" (Task 5's ask envelope) sets Approvals.
type Interrupt struct {
	Kind       string // permission | subagent_permission | ask_user | plan_approval | approvals
	Subagent   string
	Permission *PermissionInterrupt
	AskUser    *AskUserInterrupt
	Plan       *PlanInterrupt
	Approvals  *ApprovalsInterrupt
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

// AskView is one approval request of an "approvals" interrupt.
type AskView struct {
	CallID         string
	ToolName       string
	ArgsSummary    string
	ToolArgs       map[string]any
	Reason         string
	Origin         string
	SessionRuleKey string
	// Questions is set on AskUserQuestion asks (question texts, in order —
	// the answer's Answers slot matches them).
	Questions []string
	// Plan is set on ExitPlanMode asks.
	Plan string
}

// ApprovalsInterrupt is the Task-5 ask envelope: every ask of the round in
// one pause, answered together by one ApprovalAnswer.
type ApprovalsInterrupt struct {
	Asks []AskView
}

// AskDecisionView is the answer for one ask of an "approvals" interrupt.
type AskDecisionView struct {
	Outcome string // "allow" | "deny"
	Grant   string // "" | "session"
	Answers []string
}

// ApprovalAnswer is the resume value for an "approvals" interrupt: one
// decision per asked call ID.
type ApprovalAnswer struct{ Decisions map[string]AskDecisionView }

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
			switch ans := v.(type) {
			case AskUserAnswer:
				return agent.AskUserAnswer{Answers: ans.Answers}, nil
			case ApprovalAnswer:
				out := agent.ApprovalAnswer{Decisions: make(map[string]agent.AskDecision, len(ans.Decisions))}
				for id, d := range ans.Decisions {
					out.Decisions[id] = agent.AskDecision{Outcome: d.Outcome, Grant: d.Grant, Answers: d.Answers}
				}
				return out, nil
			}
			return v, nil
		}
		switch v := value.(type) {
		case agent.PermissionRequest, agent.SubagentInterrupt:
			return "n", nil
		case agent.AskUserRequest:
			return agent.AskUserAnswer{}, nil
		case agent.PlanApprovalRequest:
			return "n", nil
		case agent.ApprovalRequest:
			// Headless default: deny every ask (fail closed).
			decisions := make(map[string]agent.AskDecision, len(v.Asks))
			for _, ask := range v.Asks {
				decisions[ask.CallID] = agent.AskDecision{Outcome: agent.OutcomeDeny}
			}
			return agent.ApprovalAnswer{Decisions: decisions}, nil
		default:
			return nil, fmt.Errorf("sdk: unknown interrupt value %T", value)
		}
	}
}

func interruptFrom(value any) Interrupt {
	switch v := value.(type) {
	case agent.ApprovalRequest:
		asks := make([]AskView, 0, len(v.Asks))
		for _, a := range v.Asks {
			view := AskView{
				CallID: a.CallID, ToolName: a.ToolName, ArgsSummary: a.ArgsSummary,
				ToolArgs: a.ToolArgs, Reason: a.Reason, Origin: a.Origin,
				SessionRuleKey: a.SessionRuleKey, Plan: a.Plan,
			}
			for _, q := range a.Questions {
				view.Questions = append(view.Questions, q.Question)
			}
			asks = append(asks, view)
		}
		return Interrupt{Kind: "approvals", Approvals: &ApprovalsInterrupt{Asks: asks}}
	case agent.PermissionRequest:
		return Interrupt{Kind: "permission", Permission: &PermissionInterrupt{
			ToolName: v.ToolName, ToolArgs: v.ToolArgs, Reason: v.Reason, IsReadOnly: v.IsReadOnly,
		}}
	case agent.SubagentInterrupt:
		return Interrupt{Kind: "subagent_permission", Subagent: v.Subagent, Permission: &PermissionInterrupt{
			ToolName: v.Inner.ToolName, ToolArgs: v.Inner.ToolArgs, Reason: v.Inner.Reason, IsReadOnly: v.Inner.IsReadOnly,
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
