package server

// Session-plane admission errors must flow through the httpx error-code
// registry (api-standard §1: codes are the stable, non-localized identity;
// the frontend maps code → i18n key). The queue-full 409 and the abandoned
// 503 used to be bare http.Error/writeError replicas that carried no code —
// the web transport could only surface a context-free "409". These tests pin
// the envelope (status + code) on every answering surface: run creation,
// the gated truncate's two refusals, and the goal start's queue-full.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// codedBody decodes the standard error envelope from a response.
func codedBody(t *testing.T, resp *http.Response) (errText, code string) {
	t.Helper()
	var body struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	return body.Error, body.Code
}

// fillSessionQueue drives one holder run plus maxQueuedPerSession queued
// command runs, so the next gate waiter on sid is refused.
func fillSessionQueue(t *testing.T, ts *httptest.Server, sid string) (holderRun string) {
	t.Helper()
	holderRun, _, pos := postRun(t, ts.URL, sid, "write it")
	if pos != 0 {
		t.Fatalf("holder position = %d, want 0 (runs immediately)", pos)
	}
	for i := 0; i < maxQueuedPerSession; i++ {
		if _, _, status := postCommandRun(t, ts.URL, sid, "/undo"); status != http.StatusOK {
			t.Fatalf("/undo %d status = %d", i+1, status)
		}
	}
	return holderRun
}

// Run creation on a full session queue answers the coded 409 (S-B-0200), not
// a bare status — same for the goal start's queue-full (it used to answer
// 500 with no code).
func TestSessionQueueFullAnswersCodedConflict(t *testing.T) {
	dir := t.TempDir()
	_, ts := newGatedUndoServer(t, dir, 400*time.Millisecond)
	const sid = "s-code-queue"

	fillSessionQueue(t, ts, sid)
	resp, err := http.Post(ts.URL+"/api/runs", "application/json",
		strings.NewReader(fmt.Sprintf(`{"prompt":"one too many","session_id":%q}`, sid)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("run on full queue = %d, want 409", resp.StatusCode)
	}
	if _, code := codedBody(t, resp); code != "S-B-0200" {
		t.Fatalf("run queue-full code = %q, want S-B-0200", code)
	}

	// The goal start hits the same admission plane: its queue-full answer
	// carries the same code (and no longer a 500).
	resp, err = http.Post(ts.URL+"/api/sessions/"+sid+"/goal", "application/json",
		strings.NewReader(`{"command":"ship it"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("goal start on full queue = %d, want 409", resp.StatusCode)
	}
	if _, code := codedBody(t, resp); code != "S-B-0200" {
		t.Fatalf("goal queue-full code = %q, want S-B-0200", code)
	}
}

// The gated truncate's two refusals are coded too: queue-full (S-B-0200) and
// the abandoned-after-queuing 503 (S-B-0201) — the waiter's client went away
// before promotion, so the refusal cannot reach it, but the envelope must
// still be the standard one (recorder-driven, like the 503 regression).
func TestTruncateRefusalsAreCoded(t *testing.T) {
	dir := t.TempDir()
	s, ts := newGatedUndoServer(t, dir, 400*time.Millisecond)
	const sid = "s-code-trunc"

	// Queue-full refusal through the HTTP surface.
	fillSessionQueue(t, ts, sid)
	resp, err := http.Post(ts.URL+"/api/sessions/"+sid+"/truncate", "application/json",
		strings.NewReader(`{"keep":1}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("truncate on full queue = %d, want 409", resp.StatusCode)
	}
	if _, code := codedBody(t, resp); code != "S-B-0200" {
		t.Fatalf("truncate queue-full code = %q, want S-B-0200", code)
	}

	// Abandoned-waiter refusal through the recorder (the client canceled its
	// context and cannot read the response it caused). Fresh session with
	// just a holder: the truncate must QUEUE (not be refused at enter) so
	// the abandonment is what answers.
	const sid2 = "s-code-trunc-abandon"
	if _, _, pos := postRun(t, ts.URL, sid2, "write it"); pos != 0 {
		t.Fatalf("holder2 position = %d, want 0", pos)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/"+sid2+"/truncate",
		strings.NewReader(`{"keep":1}`))
	req.SetPathValue("id", sid2)
	ctx, cancel := context.WithCancel(context.Background())
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.handleTruncateSession(rec, req)
	}()
	time.Sleep(150 * time.Millisecond) // let the waiter join the queue
	cancel()
	<-done
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("abandoned truncate status = %d, want 503", rec.Code)
	}
	var body struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode abandoned envelope: %v", err)
	}
	if body.Code != "S-B-0201" {
		t.Fatalf("abandoned truncate code = %q, want S-B-0201", body.Code)
	}
}
