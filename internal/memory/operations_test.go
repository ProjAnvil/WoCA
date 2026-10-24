package memory

import "testing"

// The OperationEngine capability is the recall_ops surface (LadyM v0.7.0):
// structured operation records (resources + normalized action) that answer
// "how did I last operate on X?". Implemented by the embedded engine only —
// the remote engine has no HTTP route for it.

func TestRecordAndRecallOperations(t *testing.T) {
	eng := newEngine(t)
	ctx := t.Context()

	var ops OperationEngine = eng
	id, err := ops.RecordOperation(ctx, "", Operation{
		Action:    "refactor",
		Resources: []string{"internal/memory/ladym.go"},
		Summary:   "Q: refactor the engine wrapper\nA: dropped the global lock",
		Outcome:   "success",
	})
	if err != nil {
		t.Fatal(err)
	}
	if id == "" {
		t.Fatal("RecordOperation returned empty id")
	}

	hits, err := ops.RecallOperations(ctx, "", "internal/memory/ladym.go", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Fatal("RecallOperations found nothing for the recorded resource")
	}
	if hits[0].Action != "refactor" {
		t.Fatalf("hit action = %q, want refactor", hits[0].Action)
	}
	if hits[0].Outcome != "success" {
		t.Fatalf("hit outcome = %q, want success", hits[0].Outcome)
	}
	if hits[0].Summary == "" {
		t.Fatal("hit summary is empty")
	}
}

func TestRecallOperationsRequiresResource(t *testing.T) {
	eng := newEngine(t)
	var ops OperationEngine = eng
	if _, err := ops.RecallOperations(t.Context(), "", "", 5); err == nil {
		t.Fatal("an empty resource must be rejected")
	}
}

func TestRecallOperationsUnknownResourceDegrades(t *testing.T) {
	eng := newEngine(t)
	var ops OperationEngine = eng
	hits, err := ops.RecallOperations(t.Context(), "", "file-that-never-was.txt", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("an unknown resource must degrade to empty hits, got %+v", hits)
	}
}
