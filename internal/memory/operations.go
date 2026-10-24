package memory

import (
	"context"
	"errors"
	"fmt"

	ladymengine "github.com/ProjAnvil/LadyM/engine"
)

// Operation is one structured operation record: what the agent did to which
// workspace resources (LadyM v0.7.0 resource slots). Action is normalized by
// the engine onto its canonical vocabulary (read/write/…/refactor/…).
type Operation struct {
	Resources []string
	Action    string
	Summary   string
	Outcome   string
}

// OperationHit is one recall_ops result: an episode linked to the queried
// resource through the L4 operates_on graph, ACT-R ranked.
type OperationHit struct {
	ID      string
	Action  string
	Summary string
	Outcome string
	Score   float64
}

// OperationEngine is the recall_ops capability (LadyM v0.7.0): structured
// operation records answering "how did I last operate on X?". This is a
// capability interface, not part of Engine: the embedded ladym engine
// implements it; the remote data-plane has no HTTP route for operations, so
// enterprise deployments type-assert and find the feature off.
type OperationEngine interface {
	// RecordOperation stores one structured operation episode and links it
	// to workspace L2 resource entities. Best-effort callers treat errors
	// as dropped records.
	RecordOperation(ctx context.Context, workspace string, op Operation) (string, error)
	// RecallOperations queries episodes by resource (file path, symbol,
	// service name — anything the engine's resource registry resolves).
	// An unresolvable resource degrades to empty hits, not an error.
	RecallOperations(ctx context.Context, workspace, resource string, topK int) ([]OperationHit, error)
}

// RecordOperation implements OperationEngine over the embedded engine.
func (l *LadyM) RecordOperation(_ context.Context, workspace string, op Operation) (string, error) {
	scope := l.eng.Scope(l.workspace(workspace))
	m, err := scope.RecordOperation(ladymengine.OperationEvent{
		Agent:       "woca",
		Action:      op.Action,
		Resources:   op.Resources,
		Observation: op.Summary,
		Outcome:     op.Outcome,
	})
	if err != nil {
		return "", fmt.Errorf("memory: record operation: %w", err)
	}
	if m == nil {
		return "", nil
	}
	if m.Metadata["gated"] == "dropped" {
		return "", nil // dropped by the attention gate; nothing was persisted
	}
	return m.ID, nil
}

// RecallOperations implements OperationEngine over the embedded engine.
func (l *LadyM) RecallOperations(_ context.Context, workspace, resource string, topK int) ([]OperationHit, error) {
	if resource == "" {
		return nil, errors.New("memory: operations query requires a resource")
	}
	if topK <= 0 {
		topK = 10
	}
	scope := l.eng.Scope(l.workspace(workspace))
	res, err := scope.RecallOperations(ladymengine.OperationsQuery{Resource: resource, Limit: topK})
	if err != nil {
		return nil, fmt.Errorf("memory: recall operations: %w", err)
	}
	out := make([]OperationHit, 0, len(res.Items))
	for _, item := range res.Items {
		if item == nil || item.Memory == nil {
			continue
		}
		hit := hitFromMemory(item.Memory, item.Score)
		action, _ := item.Memory.Metadata["action"].(string)
		outcome, _ := item.Memory.Metadata["outcome"].(string)
		out = append(out, OperationHit{
			ID: hit.ID, Action: action, Summary: hit.Summary,
			Outcome: outcome, Score: hit.Score,
		})
	}
	return out, nil
}
