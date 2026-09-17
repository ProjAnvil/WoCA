package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRevocationCascade(t *testing.T) {
	tr := newTestRelay(t)
	client := tr.login(t)
	auditLog, err := NewAuditLog("")
	if err != nil {
		t.Fatal(err)
	}
	revoker := NewRevoker(tr.ks, tr.rl.gens, tr.rl.park, auditLog)

	// NOTE (choreography amendment vs the task-6 brief): the brief starts
	// the 30s in-flight poll BEFORE this SSE setup poll. With two
	// concurrent ParkingQueue.Take waiters and one parked client request,
	// either poll can win the item — and the FIFO-woken 30s poll wins in
	// practice, takes the item, never responds, and the test deadlocks at
	// client.Get below (verified 4/4 runs). The T5 pattern
	// (TestTunnelRoundTrip/TestTunnelSSE) keeps exactly one Take waiter
	// active when the client request parks; this test does the same —
	// the SSE stream is established first, THEN the in-flight poll parks.
	// Every assertion below is byte-identical to the brief.

	// Client: one forwarded SSE held open by the bridge across the revoke.
	hold := make(chan struct{})
	sseDone := make(chan string, 1)
	go func() {
		resp, p := tr.pollOnce(t, "gen-1", 5*time.Second)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return
		}
		pr, pw := io.Pipe()
		go func() {
			_, _ = pw.Write([]byte("data: hello\n\n"))
			<-hold // the stream stays open across the revoke
			_ = pw.Close()
		}()
		r := tr.respond(t, "gen-1", p.ID, http.StatusOK,
			http.Header{"Content-Type": {"text/event-stream"}}, pr)
		body, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		sseDone <- fmt.Sprintf("%d %s", r.StatusCode, body)
	}()
	time.Sleep(100 * time.Millisecond)

	sseResp, err := client.Get(tr.srv.URL + "/api/runs/run-1/events")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sseResp.Body.Close() }()
	first := make([]byte, 64)
	n, _ := sseResp.Body.Read(first)
	if !strings.Contains(string(first[:n]), "data: hello") {
		t.Fatalf("pre-revoke frame: %q", first[:n])
	}

	// Bridge: one long poll parked mid-wait when the revoke lands.
	pollResult := make(chan int, 1)
	go func() {
		resp, _ := tr.pollOnce(t, "gen-1", 30*time.Second)
		pollResult <- resp.StatusCode
		_ = resp.Body.Close()
	}()
	time.Sleep(100 * time.Millisecond)

	// Revoke: the CLI writes revoked_at; the sweeper cascades (design §5).
	if _, ok, err := tr.ks.RevokeKey(t.Context(), "home-mac"); err != nil || !ok {
		t.Fatal("revoke failed")
	}
	if err := revoker.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	close(hold)

	// 1. The in-flight poll answered 410 immediately — not after 30s.
	select {
	case code := <-pollResult:
		if code != http.StatusGone {
			t.Fatalf("in-flight poll after revoke: %d, want 410", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("in-flight poll not woken by revocation")
	}
	// 2. The live SSE got the injected revocation event, then closed:
	// the frame must be `event: auth_revoked` with {"code":"revoked"} —
	// exactly what web/src/api/streaming.ts:151-159 switches on.
	tail, err := io.ReadAll(sseResp.Body)
	if err != nil {
		t.Fatalf("SSE tail read: %v", err)
	}
	if !strings.Contains(string(tail), "event: auth_revoked\ndata: {\"code\":\"revoked\"}") {
		t.Fatalf("SSE tail: %q (auth_revoked frame must be injected)", tail)
	}
	// 3. The bridge's respond handler unblocked (no goroutine leak).
	select {
	case <-sseDone:
	case <-time.After(3 * time.Second):
		t.Fatal("respond handler still blocked after revoke")
	}
	// 4. The revoked session now resolves to ErrKeyRevoked through the
	// keys join (tombstoning — the session row stays so the code is
	// determinable): 401 with the frontend-contract code, cookie and
	// Bearer alike.
	resp, err := client.Get(tr.srv.URL + "/api/sessions")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized ||
		!strings.Contains(string(body), `"code":"revoked"`) {
		t.Fatalf("post-revoke API: %d %s, want 401 + code revoked", resp.StatusCode, body)
	}
	// 5. New polls get 410 with the frontend contract code.
	resp, _ = tr.pollOnce(t, "gen-2", 10*time.Millisecond)
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusGone || !strings.Contains(string(body), `"code":"revoked"`) {
		t.Fatalf("post-revoke poll: %d %s, want 410 + code revoked", resp.StatusCode, body)
	}
	// 6. The cascade is idempotent (a second sweep does nothing new).
	if err := revoker.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestParkedClientRevokedGets410 covers design §5.3: a client request
// parked (not yet picked by the bridge) when the key is revoked must be
// released WITH the revoked signal — 410 {"code":"revoked"} — never the
// bare 200 net/http auto-emits when a handler writes nothing.
func TestParkedClientRevokedGets410(t *testing.T) {
	tr := newTestRelay(t)
	client := tr.login(t)
	tr.claimGen(t, "gen-1")

	// Park one client request; no bridge poll ever takes it.
	type result struct {
		code int
		body string
	}
	parked := make(chan result, 1)
	go func() {
		resp, err := client.Get(tr.srv.URL + "/api/sessions")
		if err != nil {
			parked <- result{code: -1, body: err.Error()}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		parked <- result{code: resp.StatusCode, body: string(body)}
	}()
	time.Sleep(100 * time.Millisecond) // let the request park
	if n := tr.rl.park.Len(tr.key.ID); n != 1 {
		t.Fatalf("parked requests: %d, want 1", n)
	}

	// Revoke and sweep: the parked item drains with ErrRevoked.
	if _, ok, err := tr.ks.RevokeKey(t.Context(), "home-mac"); err != nil || !ok {
		t.Fatal("revoke failed")
	}
	auditLog, err := NewAuditLog("")
	if err != nil {
		t.Fatal(err)
	}
	if err := NewRevoker(tr.ks, tr.rl.gens, tr.rl.park, auditLog).Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}

	select {
	case got := <-parked:
		if got.code != http.StatusGone ||
			!strings.Contains(got.body, `"code":"revoked"`) {
			t.Fatalf("parked client after revoke: %d %s, want 410 + code revoked (bare 200 hides the revocation)",
				got.code, got.body)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("parked client not released by revocation")
	}
}

func TestAuditLogAppend(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	a, err := NewAuditLog(path)
	if err != nil {
		t.Fatal(err)
	}
	a.Event(AuditLoginFailed, "", "1.2.3.4")
	a.Event(AuditRevocation, "home-mac", "key revoked")
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("audit lines: %d, want 2", len(lines))
	}
	for i, line := range lines {
		var entry struct {
			Ts     string `json:"ts"`
			Event  string `json:"event"`
			Key    string `json:"key"`
			Detail string `json:"detail"`
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("line %d not JSON: %v", i, err)
		}
		if entry.Ts == "" || entry.Event == "" {
			t.Fatalf("line %d incomplete: %+v", i, entry)
		}
	}
	// Append-only: a second open adds, never truncates (design §5.4).
	a2, err := NewAuditLog(path)
	if err != nil {
		t.Fatal(err)
	}
	a2.Event(AuditBridgeUp, "home-mac", "")
	if err := a2.Close(); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(path)
	if got := len(strings.Split(strings.TrimSpace(string(data)), "\n")); got != 3 {
		t.Fatalf("audit not append-only: %d lines, want 3", got)
	}
}

func TestLoginAuditEvents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	auditLog, err := NewAuditLog(path)
	if err != nil {
		t.Fatal(err)
	}
	ks := openTestKeyStore(t)
	_, raw, err := ks.CreateKey(t.Context(), "home-mac")
	if err != nil {
		t.Fatal(err)
	}
	a := NewAuthService(ks, false, testLogger())
	a.Audit = auditLog

	loginFor(t, a, "wcr_"+strings.Repeat("A", 43)) // failure
	loginFor(t, a, raw)                            // success
	_ = auditLog.Close()

	data, _ := os.ReadFile(path)
	out := string(data)
	if !strings.Contains(out, `"event":"login_failed"`) {
		t.Fatalf("login failure not audited: %s", out)
	}
	if !strings.Contains(out, `"event":"login_ok"`) {
		t.Fatalf("login success not audited: %s", out)
	}
	if strings.Contains(out, raw) {
		t.Fatal("audit log leaked the full key (metadata only, design §4.1)")
	}
}

func TestRevokerSweepStoreError(t *testing.T) {
	ks := openTestKeyStore(t)
	auditLog, err := NewAuditLog("")
	if err != nil {
		t.Fatal(err)
	}
	revoker := NewRevoker(ks, NewGenerationRegistry(), NewParkingQueue(1), auditLog)
	if err := ks.Close(); err != nil {
		t.Fatal(err)
	}
	if err := revoker.Sweep(context.Background()); err == nil {
		t.Fatal("sweep against a closed store must fail")
	}
}

func TestRevokerWatch(t *testing.T) {
	ks := openTestKeyStore(t)
	ctx := context.Background()
	k, _, err := ks.CreateKey(ctx, "watched")
	if err != nil {
		t.Fatal(err)
	}
	gens := NewGenerationRegistry()
	park := NewParkingQueue(1)
	auditLog, err := NewAuditLog("")
	if err != nil {
		t.Fatal(err)
	}
	revoker := NewRevoker(ks, gens, park, auditLog)

	watchCtx, stop := context.WithCancel(context.Background())
	defer stop()
	done := make(chan struct{})
	go func() {
		revoker.Watch(watchCtx, 5*time.Millisecond)
		close(done)
	}()

	// A revoke written by a separate CLI process (only the row changes) is
	// picked up by the sweeping relay.
	if _, ok, err := ks.RevokeKey(ctx, "watched"); err != nil || !ok {
		t.Fatalf("revoke setup: err=%v ok=%v", err, ok)
	}
	deadline := time.After(2 * time.Second)
	for {
		revoker.mu.Lock()
		handled := revoker.done[k.ID]
		revoker.mu.Unlock()
		if handled {
			break
		}
		select {
		case <-deadline:
			t.Fatal("watch never swept the revoked key")
		case <-time.After(5 * time.Millisecond):
		}
	}
	// Context cancellation ends the watch loop.
	stop()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watch did not stop on context cancel")
	}
}
