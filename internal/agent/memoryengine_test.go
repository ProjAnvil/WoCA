package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/memory"
	"github.com/projanvil/woca/internal/settings"
)

// fakeEngine is a minimal memory.Engine for agent-side tests (shared by later tasks).
type fakeEngine struct {
	mu            sync.Mutex
	recalls       []string
	topKs         []int
	remembered    []string
	rememberCalls []string // "ws\x00src\x00content" per Remember call
	events        []string
	eventObs      []string // observation per RecordEvent call (parallel to events)
	eventWSs      []string // workspace per RecordEvent call (parallel to events)
	hits          []memory.Hit
	consolidates  int                // number of Consolidate calls (System 2 cycles)
	consSince     []float64          // since argument per Consolidate call
	err           bool               // Recall returns an error when set
	drop          bool               // Remember returns ("", nil): attention-gate drop
	rememberErr   error              // Remember returns this error when set (overrides drop)
	eventErr      error              // RecordEvent returns this error when set (still recorded)
	ops           []memory.Operation // RecordOperation calls (recall_ops capability)
	opErr         error              // RecordOperation returns this error when set (still recorded)
}

func (f *fakeEngine) Recall(ctx context.Context, query, workspace string, topK int) ([]memory.Hit, error) {
	f.mu.Lock()
	f.recalls = append(f.recalls, workspace+"|"+query)
	f.topKs = append(f.topKs, topK)
	f.mu.Unlock()
	if f.err {
		return nil, errors.New("boom")
	}
	return f.hits, nil
}
func (f *fakeEngine) SearchCode(ctx context.Context, q, ws string, k int) ([]memory.Hit, error) {
	return nil, nil
}
func (f *fakeEngine) Remember(ctx context.Context, ws, content string, tags []string, src string) (string, error) {
	f.mu.Lock()
	f.remembered = append(f.remembered, content)
	f.rememberCalls = append(f.rememberCalls, ws+"\x00"+src+"\x00"+content)
	f.mu.Unlock()
	if f.rememberErr != nil {
		return "", f.rememberErr
	}
	if f.drop {
		return "", nil // attention gate dropped the content
	}
	return "id-1", nil
}
func (f *fakeEngine) RecordEvent(ctx context.Context, ws, agentName, action, observation, outcome string) error {
	f.mu.Lock()
	f.events = append(f.events, agentName+"/"+action+"/"+outcome)
	f.eventObs = append(f.eventObs, observation)
	f.eventWSs = append(f.eventWSs, ws)
	f.mu.Unlock()
	return f.eventErr
}
func (f *fakeEngine) List(ctx context.Context, ws, layer string, limit, offset int) ([]memory.Hit, error) {
	return nil, nil
}
func (f *fakeEngine) Forget(ctx context.Context, id string) error { return nil }
func (f *fakeEngine) Stats(ctx context.Context, ws string) (*memory.Stats, error) {
	return &memory.Stats{Total: 0, Layers: map[string]int{}}, nil
}
func (f *fakeEngine) IndexCode(ctx context.Context, root, ws string, force bool) (string, error) {
	return "", nil
}
func (f *fakeEngine) Consolidate(ctx context.Context, ws string, since float64) (string, error) {
	f.mu.Lock()
	f.consolidates++
	f.consSince = append(f.consSince, since)
	f.mu.Unlock()
	return "cycle", nil
}
func (f *fakeEngine) Description() string { return "fake" }
func (f *fakeEngine) Close() error        { return nil }

func (f *fakeEngine) RecordOperation(ctx context.Context, ws string, op memory.Operation) (string, error) {
	f.mu.Lock()
	f.ops = append(f.ops, op)
	f.mu.Unlock()
	if f.opErr != nil {
		return "", f.opErr
	}
	return "op-1", nil
}

func (f *fakeEngine) RecallOperations(ctx context.Context, ws, resource string, topK int) ([]memory.OperationHit, error) {
	return nil, nil
}

// recordedTopKs returns a copy of the Recall topK arguments (the prefetch
// goroutine appends concurrently; test goroutines must not read f.topKs
// directly).
func (f *fakeEngine) recordedTopKs() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.topKs...)
}

// recordedRecalls returns a copy of the "workspace|query" Recall records.
func (f *fakeEngine) recordedRecalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.recalls...)
}

// recordedRememberCalls returns a copy of the "ws\x00src\x00content" Remember records.
func (f *fakeEngine) recordedRememberCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.rememberCalls...)
}

// recordedEventWSs returns a copy of the per-RecordEvent workspaces.
func (f *fakeEngine) recordedEventWSs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.eventWSs...)
}

// recordedEventObs returns a copy of the per-RecordEvent observations.
func (f *fakeEngine) recordedEventObs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.eventObs...)
}

// recordedConsolidateSinces returns a copy of the per-Consolidate since
// arguments (the System 2 goroutine appends concurrently).
func (f *fakeEngine) recordedConsolidateSinces() []float64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]float64(nil), f.consSince...)
}

// recordedConsolidates returns the number of Consolidate calls (the System 2
// goroutine appends concurrently; test goroutines must not read directly).
func (f *fakeEngine) recordedConsolidates() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.consolidates
}

func TestPrefetchUsesEngineRecall(t *testing.T) {
	f := &fakeEngine{hits: []memory.Hit{{
		ID: "h1", Layer: "L2_semantic", Type: "fact",
		Summary: "auth uses JWT", Content: "auth uses JWT with 24h expiry",
		Score: 0.83, UpdatedAt: float64(time.Now().Unix()),
	}, {
		ID: "h2", Layer: "L2_semantic", Type: "fact",
		Summary: "no timestamp hit", Content: "no timestamp hit",
		Score: 0.7,
	}}}
	a := &Agent{mem: memoryRuntime{engine: f}, workdir: "/ws"}
	a.SetMemoryWorkspace("/ws/custom")
	a.beginMemoryPrefetch(t.Context(), []messages.Message{messages.Human("how does auth work?")})
	var got []messages.Message
	for i := 0; i < 50 && len(got) < 2; i++ {
		got = a.takeMemoryAttachments(t.Context())
		if len(got) < 2 {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if len(got) != 2 {
		t.Fatalf("attachments = %d, want 2", len(got))
	}
	body := got[0].Content
	if !strings.Contains(body, "<relevant-memory>") || !strings.Contains(body, "[L2_semantic|fact|0.83|") {
		t.Fatalf("attachment format: %q", body)
	}
	if !strings.Contains(body, "|just now] ") {
		t.Fatalf("attachment header should carry a relative age: %q", body)
	}
	if !strings.Contains(body, "auth uses JWT") {
		t.Fatalf("attachment content: %q", body)
	}
	// A hit without a timestamp omits the age field entirely.
	if b2 := got[1].Content; !strings.Contains(b2, "[L2_semantic|fact|0.70] ") || strings.Count(b2, "|") != 2 {
		t.Fatalf("no-timestamp attachment format: %q", b2)
	}
	if recalls := f.recordedRecalls(); len(recalls) != 1 || recalls[0] != "/ws/custom|how does auth work?" {
		t.Fatalf("recalls = %v", f.recordedRecalls())
	}
	if more := a.takeMemoryAttachments(t.Context()); len(more) != 0 {
		t.Fatalf("dedup failed: %v", more)
	}
}

func TestPrefetchEngineTopKFromSettings(t *testing.T) {
	f := &fakeEngine{}
	a := &Agent{
		mem: memoryRuntime{
			engine: f,
			loadSettings: func() settings.Settings {
				s := settings.Default()
				s.LadyMTopK = 12
				return s
			},
		},
		workdir: "/ws",
	}
	a.beginMemoryPrefetch(t.Context(), []messages.Message{messages.Human("q")})
	deadline := time.Now().Add(2 * time.Second)
	for len(f.recordedTopKs()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if topKs := f.recordedTopKs(); len(topKs) != 1 || topKs[0] != 12 {
		t.Fatalf("topKs = %v", topKs)
	}
}

func TestMemoryWorkspaceDefaultsToWorkdir(t *testing.T) {
	a := &Agent{workdir: "/ws/default"}
	if ws := a.memoryWorkspace(); ws != "/ws/default" {
		t.Fatalf("workspace = %q", ws)
	}
	a.SetMemoryWorkspace("/ws/override")
	if ws := a.memoryWorkspace(); ws != "/ws/override" {
		t.Fatalf("workspace = %q", ws)
	}
	a.SetMemoryWorkspace("") // empty ignored
	if ws := a.memoryWorkspace(); ws != "/ws/override" {
		t.Fatalf("empty set should be ignored, got %q", ws)
	}
}

func TestEngineRecallErrorSkipsSilently(t *testing.T) {
	f := &fakeEngine{err: true}
	a := &Agent{mem: memoryRuntime{engine: f}, workdir: "/ws"}
	a.beginMemoryPrefetch(t.Context(), []messages.Message{messages.Human("q")})
	time.Sleep(150 * time.Millisecond)
	if recalls := f.recordedRecalls(); len(recalls) != 1 {
		t.Fatalf("error path should have executed exactly one Recall, got %v", recalls)
	}
	if got := a.takeMemoryAttachments(t.Context()); len(got) != 0 {
		t.Fatalf("error path should produce no attachments: %v", got)
	}
}

func TestClampUTF8(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"", 10, ""},                     // empty
		{"short", 10, "short"},           // under the limit
		{"exactly10!", 10, "exactly10!"}, // at the limit
		{"0123456789abcdef", 10, "0123456789"},
		{"aut-aut-aut-aut", 4, "aut-"}, // ASCII boundary
		{"あいうえおか", 9, "あいう"},           // 3-byte runes, n=9 is a boundary
		{"あいうえおか", 8, "あい"},            // n=8 splits a rune: back off to 6
		{"あいうえおか", 7, "あい"},            // back off to 6
		{"あいうえおか", 2, ""},              // backs off to empty
		{"あいうえおか", 1, ""},              // no complete rune fits
		{"あaあaあa", 5, "あa"},            // mixed widths
		{"anything", -1, ""},           // negative n
	}
	for _, c := range cases {
		if got := clampUTF8(c.in, c.n); got != c.want {
			t.Errorf("clampUTF8(%q, %d) = %q, want %q", c.in, c.n, got, c.want)
		}
		if n := len(c.want); n > max(c.n, 0) {
			t.Errorf("clampUTF8(%q, %d) exceeded n", c.in, c.n)
		}
	}
}

// TestMemoryRuntimeStandalone pins the component contract: prefetch and
// retention work on the runtime alone, with the workspace handed in and no
// Agent involved.
func TestMemoryRuntimeStandalone(t *testing.T) {
	f := &fakeEngine{hits: []memory.Hit{{ID: "h1", Layer: "L2", Type: "fact", Summary: "auth uses oauth2", Score: 0.9}}}
	m := memoryRuntime{engine: f}
	m.Begin(t.Context(), []messages.Message{messages.Human("how does auth work?")}, "/ws")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(f.recordedRecalls()) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if recalls := f.recordedRecalls(); len(recalls) != 1 || recalls[0] != "/ws|how does auth work?" {
		t.Fatalf("recalls = %v", recalls)
	}
	atts := m.TakeAttachments(t.Context())
	if len(atts) != 1 || !strings.Contains(atts[0].Content, "auth uses oauth2") {
		t.Fatalf("attachments = %v", atts)
	}
	// Retention: engine swap is visible through the same snapshot path.
	m.RetainRun(t.Context(), "/ws",
		[]messages.Message{messages.Human("q")}, []messages.Message{messages.AI("a")}, nil, nil)
	if len(f.remembered) != 0 {
		t.Fatalf("auto-retain must not write facts directly: %v", f.remembered)
	}
	if obs := f.recordedEventObs(); len(obs) != 1 || !strings.Contains(obs[0], "Q: q") || !strings.Contains(obs[0], "A: a") {
		t.Fatalf("episode observations = %v", obs)
	}
}
