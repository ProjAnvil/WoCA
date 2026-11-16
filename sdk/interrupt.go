package sdk

import (
	"fmt"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/runner"
)

// Interrupt is the public view of a graph pause: Kind selects the payload.
// "subagent_permission" sets Subagent plus the inner Permission;
// "approvals" (Task 5's ask envelope) sets Approvals; "multi" (Task 8)
// sets Interrupts — every pending card of the pause, each with its NS key.
type Interrupt struct {
	Kind       string // permission | subagent_permission | ask_user | plan_approval | approvals | multi
	NS         string // the card's fork key: the map-answer address (multi pauses)
	Subagent   string
	Permission *PermissionInterrupt
	AskUser    *AskUserInterrupt
	Plan       *PlanInterrupt
	Approvals  *ApprovalsInterrupt
	Interrupts []Interrupt
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

// CardAnswer answers one card of a multi-card pause: Key is the card's NS
// (Interrupt.NS), Value its answer (ApprovalAnswer, AskUserAnswer, bool or
// string per kind).
type CardAnswer struct {
	Key   string
	Value any
}

// ListAnswer is the resume value for a multi-card pause: one entry per
// answered card. Cards without an entry re-pause.
type ListAnswer []CardAnswer

// interruptHandler returns the runner handler: the caller's OnInterrupt when
// set, else the headless default matching the CLI's non-interactive posture
// (deny permissions, empty answers, reject plans). A pause's cards arrive
// as ONE Interrupt (the aggregate view for several pending cards).
func (o Options) interruptHandler() runner.InterruptHandler {
	return func(cards []agent.PauseCard) (any, error) {
		if len(cards) == 0 {
			return nil, fmt.Errorf("sdk: pause carries no interrupts")
		}
		for _, c := range cards {
			switch c.Value.(type) {
			case agent.PermissionRequest, agent.SubagentInterrupt, agent.PlanApprovalRequest, agent.AskUserRequest, agent.ApprovalRequest:
			default:
				return nil, fmt.Errorf("sdk: unknown interrupt value %T", c.Value)
			}
		}
		if o.OnInterrupt != nil {
			v, err := o.OnInterrupt(interruptFromCards(cards))
			if err != nil {
				return nil, err
			}
			switch ans := v.(type) {
			case ListAnswer:
				out := make(map[string]any, len(ans))
				for _, ca := range ans {
					out[ca.Key] = translateAnswer(ca.Value)
				}
				return out, nil
			case map[string]any:
				out := make(map[string]any, len(ans))
				for k, val := range ans {
					out[k] = translateAnswer(val)
				}
				return out, nil
			default:
				return translateAnswer(v), nil
			}
		}
		if len(cards) == 1 {
			return headlessAnswer(cards[0]), nil
		}
		// Headless default: fail closed on every card of the pause.
		out := make(map[string]any, len(cards))
		for _, c := range cards {
			out[agent.CardKey(c)] = headlessAnswer(c)
		}
		return out, nil
	}
}

// translateAnswer maps sdk answer views onto the agent resume types;
// anything else passes through (bools, strings, raw maps for passthrough
// callers).
func translateAnswer(v any) any {
	switch ans := v.(type) {
	case AskUserAnswer:
		return agent.AskUserAnswer{Answers: ans.Answers}
	case ApprovalAnswer:
		out := agent.ApprovalAnswer{Decisions: make(map[string]agent.AskDecision, len(ans.Decisions))}
		for id, d := range ans.Decisions {
			out.Decisions[id] = agent.AskDecision{Outcome: d.Outcome, Grant: d.Grant, Answers: d.Answers}
		}
		return out
	}
	return v
}

// headlessAnswer is the fail-closed answer for one card.
func headlessAnswer(card agent.PauseCard) any {
	switch v := card.Value.(type) {
	case agent.AskUserRequest:
		return agent.AskUserAnswer{}
	case agent.ApprovalRequest:
		decisions := make(map[string]agent.AskDecision, len(v.Asks))
		for _, ask := range v.Asks {
			decisions[ask.CallID] = agent.AskDecision{Outcome: agent.OutcomeDeny}
		}
		return agent.ApprovalAnswer{Decisions: decisions}
	default:
		return "n"
	}
}

// interruptFromCards builds the public Interrupt of a pause: the single
// card's view (with its NS key) when one is pending, else the "multi"
// aggregate carrying every card.
func interruptFromCards(cards []agent.PauseCard) Interrupt {
	if len(cards) == 1 {
		i := interruptFrom(cards[0].Value)
		i.NS = agent.CardKey(cards[0])
		return i
	}
	agg := Interrupt{Kind: "multi"}
	for _, c := range cards {
		i := interruptFrom(c.Value)
		i.NS = agent.CardKey(c)
		agg.Interrupts = append(agg.Interrupts, i)
	}
	return agg
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
