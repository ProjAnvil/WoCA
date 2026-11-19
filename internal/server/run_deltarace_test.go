package server

// T5 Part B: concurrent-run delta cross-contamination repro. Two sessions
// stream concurrently over one shared agent/server; each session's live
// delta events and persisted transcript must contain ONLY its own text.
//
// Case closed 2026-11-19: the original report could not be reproduced on
// any plane — server-side here (8 concurrent runs, -race, both storage
// forms), the web stream store (e2e/tests/personal/deltarace.spec.ts: two
// browser contexts streaming concurrently, including a mid-run reload,
// reattach and session switch — the sid-closure suspicion), or the
// forwarding paths (edge parked-request/respond pairing, bridge tunnel
// payload-ID pairing and the personal relay are all request-scoped;
// audited alongside). The report predates the wave1 cancel-semantics
// fixes and is deemed stale. This guard stays as the regression pin.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/rs/zerolog"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"
	"github.com/projanvil/langchain-golang/core/runnables"
	"github.com/projanvil/langchain-golang/core/schema"
	coretools "github.com/projanvil/langchain-golang/core/tools"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/tools"
)

// echoKeyModel is transcript-keyed: the answer echoes the marker found in
// the conversation (MARK-ALPHA → "alpha answer …", MARK-BETA → "beta answer
// …"). A shared stateless model — exactly what two concurrent sessions see
// in serve mode; any cross-wiring of streams would put the wrong marker's
// text in a session's events or transcript.
type echoKeyModel struct{}

func (m *echoKeyModel) pick(input []messages.Message) messages.Message {
	joined := strings.Builder{}
	for _, msg := range input {
		joined.WriteString(msg.Content)
		joined.WriteString("\n")
	}
	if strings.Contains(joined.String(), "MARK-BETA") {
		return messages.AI("beta answer BETA-TEXT-9")
	}
	return messages.AI("alpha answer ALPHA-TEXT-7")
}

func (m *echoKeyModel) Invoke(_ context.Context, input []messages.Message, _ ...runnables.Option) (messages.Message, error) {
	return m.pick(input), nil
}

func (m *echoKeyModel) Stream(_ context.Context, input []messages.Message, _ ...runnables.Option) (runnables.Stream[messages.Message], error) {
	return runnables.NewSliceStream([]messages.Message{m.pick(input)}), nil
}

// slowEchoModel streams its answer in many small deltas with pauses, so
// concurrent runs genuinely overlap in the streaming window.
type slowEchoModel struct{}

func (m *slowEchoModel) marker(input []messages.Message) string {
	for _, msg := range input {
		if strings.Contains(msg.Content, "MARK-BETA") {
			return "BETA"
		}
	}
	return "ALPHA"
}

func (m *slowEchoModel) Invoke(_ context.Context, input []messages.Message, _ ...runnables.Option) (messages.Message, error) {
	mk := m.marker(input)
	var b strings.Builder
	for i := 0; i < 20; i++ {
		fmt.Fprintf(&b, "%s-delta-%02d ", mk, i)
	}
	return messages.AI(b.String()), nil
}

func (m *slowEchoModel) Stream(ctx context.Context, input []messages.Message, _ ...runnables.Option) (runnables.Stream[messages.Message], error) {
	return &timedChunkStream{marker: m.marker(input)}, nil
}

// timedChunkStream is a pull-based stream that yields 20 marker-tagged
// chunks with a small pause each, so concurrent runs overlap in the
// streaming window.
type timedChunkStream struct {
	marker string
	next   int
}

func (s *timedChunkStream) Next(ctx context.Context) (messages.Message, bool, error) {
	if s.next >= 20 {
		return messages.Message{}, false, nil
	}
	select {
	case <-ctx.Done():
		return messages.Message{}, false, ctx.Err()
	case <-time.After(5 * time.Millisecond):
	}
	i := s.next
	s.next++
	return messages.AI(fmt.Sprintf("%s-delta-%02d ", s.marker, i)), true, nil
}

func (s *timedChunkStream) Close() error {
	s.next = 20
	return nil
}

func (m *slowEchoModel) Batch(ctx context.Context, inputs [][]messages.Message, opts ...runnables.Option) ([]messages.Message, error) {
	out := make([]messages.Message, len(inputs))
	for i, in := range inputs {
		out[i], _ = m.Invoke(ctx, in, opts...)
	}
	return out, nil
}

func (m *slowEchoModel) InputSchema() schema.Schema  { return schema.Schema{"type": "array"} }
func (m *slowEchoModel) OutputSchema() schema.Schema { return schema.Schema{"type": "object"} }
func (m *slowEchoModel) BindTools([]coretools.Tool) (language.ChatModel, error) {
	return m, nil
}
func (m *slowEchoModel) Capabilities() language.ChatModelCapabilities {
	return language.ChatModelCapabilities{ToolCalling: true, Streaming: true}
}

func (m *echoKeyModel) Batch(ctx context.Context, inputs [][]messages.Message, opts ...runnables.Option) ([]messages.Message, error) {
	out := make([]messages.Message, len(inputs))
	for i, in := range inputs {
		out[i], _ = m.Invoke(ctx, in, opts...)
	}
	return out, nil
}

func (m *echoKeyModel) InputSchema() schema.Schema  { return schema.Schema{"type": "array"} }
func (m *echoKeyModel) OutputSchema() schema.Schema { return schema.Schema{"type": "object"} }
func (m *echoKeyModel) BindTools([]coretools.Tool) (language.ChatModel, error) {
	return m, nil
}
func (m *echoKeyModel) Capabilities() language.ChatModelCapabilities {
	return language.ChatModelCapabilities{ToolCalling: true, Streaming: true}
}

// collectDeltas subscribes to a run's SSE stream and returns the joined
// delta text once the run reaches a terminal event.
func collectDeltas(t *testing.T, events <-chan t5SSE, runID string) (deltas string, terminal string) {
	t.Helper()
	deadline := time.After(30 * time.Second)
	var b strings.Builder
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return b.String(), "stream-end"
			}
			switch ev.Type {
			case "delta":
				var payload struct {
					Text string `json:"text"`
				}
				if err := json.Unmarshal([]byte(ev.Data), &payload); err == nil {
					b.WriteString(payload.Text)
				}
			case "done", "error":
				return b.String(), ev.Type
			}
		case <-deadline:
			t.Fatalf("run %s: timed out waiting for the terminal event", runID)
		}
	}
}

// TestConcurrentRunDeltaIsolation: two runs on two sessions stream at the
// same time; each session's delta stream and persisted transcript must
// carry only its own marker's text.
func TestConcurrentRunDeltaIsolation(t *testing.T) {
	workdir := t.TempDir()
	a, err := agent.New(agent.Config{
		Model:          &echoKeyModel{},
		Registry:       tools.NewRegistry(workdir),
		SystemPrompt:   "t",
		PermissionMode: config.ModeBypass,
		WorkDir:        workdir,
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := newTestServer(t, Config{Agent: a, WorkDir: workdir, Static: fstest.MapFS{}, Logger: zerolog.Nop()})
	defer ts.Close()

	runA, sidA := createRunWithPrompt(t, ts.URL, "", "use MARK-ALPHA")
	eventsA := t5Events(t, ts.URL, runA, 60*time.Second)
	runB, sidB := createRunWithPrompt(t, ts.URL, "", "use MARK-BETA")
	eventsB := t5Events(t, ts.URL, runB, 60*time.Second)
	if sidA == sidB {
		t.Fatalf("createRun reused session %q", sidA)
	}

	var wg sync.WaitGroup
	var deltasA, deltasB, termA, termB string
	wg.Add(2)
	go func() { defer wg.Done(); deltasA, termA = collectDeltas(t, eventsA, runA) }()
	go func() { defer wg.Done(); deltasB, termB = collectDeltas(t, eventsB, runB) }()
	wg.Wait()
	if termA != "done" || termB != "done" {
		t.Fatalf("terminals = %q/%q, want done/done", termA, termB)
	}

	// Live deltas: each run's stream carries only its own marker's answer.
	if strings.Contains(deltasA, "BETA-TEXT-9") {
		t.Fatalf("session A's delta stream leaked B's text: %q", deltasA)
	}
	if strings.Contains(deltasB, "ALPHA-TEXT-7") {
		t.Fatalf("session B's delta stream leaked A's text: %q", deltasB)
	}
	if !strings.Contains(deltasA, "ALPHA-TEXT-7") || !strings.Contains(deltasB, "BETA-TEXT-9") {
		t.Fatalf("missing own text: A=%q B=%q", deltasA, deltasB)
	}

	// Persisted transcripts: same isolation, plus each session's own prompt.
	for _, tc := range []struct{ sid, own, other, label string }{
		{sidA, "ALPHA-TEXT-7", "BETA-TEXT-9", "A"},
		{sidB, "BETA-TEXT-9", "ALPHA-TEXT-7", "B"},
	} {
		sess := getSession(t, ts.URL, tc.sid)
		if !strings.Contains(sess, tc.own) {
			t.Fatalf("session %s transcript missing its own text: %s", tc.label, sess)
		}
		if strings.Contains(sess, tc.other) {
			t.Fatalf("session %s transcript contaminated with the other run's text: %s", tc.label, sess)
		}
	}
}

// getSession fetches a session's persisted transcript as raw JSON.
func getSession(t *testing.T, base, sid string) string {
	t.Helper()
	resp, err := http.Get(base + "/api/sessions/" + sid)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// createRunWithPrompt posts a run with an explicit prompt.
func createRunWithPrompt(t *testing.T, base, sessionID, prompt string) (string, string) {
	t.Helper()
	body := fmt.Sprintf(`{"prompt":%q,"session_id":%q}`, prompt, sessionID)
	resp, err := http.Post(base+"/api/runs", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var created struct {
		RunID     string `json:"run_id"`
		SessionID string `json:"session_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if created.RunID == "" || created.SessionID == "" {
		t.Fatal("empty run_id/session_id")
	}
	return created.RunID, created.SessionID
}

// TestConcurrentRunDeltaIsolationStress: eight sessions run concurrently
// over a slowly-streaming shared model; every session's delta stream and
// persisted transcript must carry only its own marker.
func TestConcurrentRunDeltaIsolationStress(t *testing.T) {
	for _, tc := range []struct {
		name   string
		ledger bool
	}{
		{name: "memory"},
		{name: "ledger-sqlite", ledger: true},
	} {
		t.Run(tc.name, func(t *testing.T) { runDeltaIsolationStress(t, tc.ledger) })
	}
}

func runDeltaIsolationStress(t *testing.T, withLedger bool) {
	workdir := t.TempDir()
	a, err := agent.New(agent.Config{
		Model:          &slowEchoModel{},
		Registry:       tools.NewRegistry(workdir),
		SystemPrompt:   "t",
		PermissionMode: config.ModeBypass,
		WorkDir:        workdir,
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Agent: a, WorkDir: workdir, Static: fstest.MapFS{}, Logger: zerolog.Nop()}
	if withLedger {
		// The full serve form: sqlite session store + the run ledger whose
		// run_events mirror backs cross-instance SSE replay.
		cfg.SessionDir = t.TempDir()
		cfg.RunLedger = openSharedLedger(t)
	}
	ts := newTestServer(t, cfg)
	defer ts.Close()

	type job struct {
		runID, sid, marker string
		events             <-chan t5SSE
	}
	jobs := make([]job, 0, 8)
	for i := 0; i < 8; i++ {
		marker := "ALPHA"
		prompt := "use MARK-ALPHA"
		if i%2 == 1 {
			marker = "BETA"
			prompt = "use MARK-BETA"
		}
		runID, sid := createRunWithPrompt(t, ts.URL, "", prompt)
		jobs = append(jobs, job{runID: runID, sid: sid, marker: marker, events: t5Events(t, ts.URL, runID, 60*time.Second)})
	}

	var wg sync.WaitGroup
	results := make([]struct {
		deltas, terminal string
	}, len(jobs))
	wg.Add(len(jobs))
	for i := range jobs {
		go func(i int) {
			defer wg.Done()
			results[i].deltas, results[i].terminal = collectDeltas(t, jobs[i].events, jobs[i].runID)
		}(i)
	}
	wg.Wait()

	for i, j := range jobs {
		other := "BETA"
		if j.marker == "BETA" {
			other = "ALPHA"
		}
		if results[i].terminal != "done" {
			t.Fatalf("run %s: terminal = %q", j.runID, results[i].terminal)
		}
		if strings.Contains(results[i].deltas, other+"-delta-") {
			t.Fatalf("run %s (%s): delta stream leaked %s text: %q", j.runID, j.marker, other, results[i].deltas)
		}
		if !strings.Contains(results[i].deltas, j.marker+"-delta-19") {
			t.Fatalf("run %s (%s): delta stream incomplete: %q", j.runID, j.marker, results[i].deltas)
		}
		// The done event precedes the completion save (completeRun runs
		// after finish); poll briefly for the persisted transcript.
		var sess string
		deadline := time.Now().Add(5 * time.Second)
		for {
			sess = getSession(t, ts.URL, j.sid)
			if strings.Contains(sess, j.marker+"-delta-19") || time.Now().After(deadline) {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if strings.Contains(sess, other+"-delta-") {
			t.Fatalf("session %s (%s): transcript leaked %s text: %s", j.sid, j.marker, other, sess)
		}
		if !strings.Contains(sess, j.marker+"-delta-19") {
			t.Fatalf("session %s (%s): transcript missing its own text: %s", j.sid, j.marker, sess)
		}
	}
}
