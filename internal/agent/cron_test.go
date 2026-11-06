package agent

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/tools"
	"github.com/projanvil/woca/internal/tracing"
)

func TestCronParser(t *testing.T) {
	cases := []struct {
		spec  string
		ok    bool
		match time.Time // a time that should (ok) or should not (!ok) match
	}{
		{"*/5 * * * *", true, time.Date(2026, 8, 15, 10, 5, 0, 0, time.Local)},
		{"*/5 * * * *", false, time.Date(2026, 8, 15, 10, 7, 0, 0, time.Local)},
		{"0 9 * * 1-5", true, time.Date(2026, 8, 17, 9, 0, 0, 0, time.Local)},  // Monday
		{"0 9 * * 1-5", false, time.Date(2026, 8, 15, 9, 0, 0, 0, time.Local)}, // Saturday
		{"30 14 1 * *", true, time.Date(2026, 9, 1, 14, 30, 0, 0, time.Local)},
		{"15,45 * * * *", true, time.Date(2026, 8, 15, 12, 45, 0, 0, time.Local)},
		{"15,45 * * * *", false, time.Date(2026, 8, 15, 12, 44, 0, 0, time.Local)},
		{"* * * *", false, time.Time{}},    // wrong field count
		{"99 * * * *", false, time.Time{}}, // out of range
		{"* * * 1-12 *", true, time.Date(2026, 3, 3, 3, 3, 0, 0, time.Local)},
	}
	for _, tc := range cases {
		m, err := parseCron(tc.spec)
		if tc.ok {
			if err != nil {
				t.Errorf("parseCron(%q): %v", tc.spec, err)
				continue
			}
			if !m.matches(tc.match) {
				t.Errorf("%q should match %v", tc.spec, tc.match)
			}
		} else if tc.spec == "* * * *" || tc.spec == "99 * * * *" {
			if err == nil {
				t.Errorf("parseCron(%q) should fail", tc.spec)
			}
		} else {
			if err != nil {
				t.Fatalf("parseCron(%q): %v", tc.spec, err)
			}
			if m.matches(tc.match) {
				t.Errorf("%q should NOT match %v", tc.spec, tc.match)
			}
		}
	}
}

func newCronAgent(t *testing.T) *Agent {
	t.Helper()
	a, err := New(Config{
		Model:          language.NewFakeChatModel(language.WithResponses(messages.AI("ok"))),
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "t",
		PermissionMode: config.ModeBypass,
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestScheduleCronFiresNotifications(t *testing.T) {
	a := newCronAgent(t)
	id, err := a.addCron(t.Context(), "check", "run the checks", "* * * * *")
	if err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 8, 15, 10, 30, 12, 0, time.Local)
	a.fireDueCron(now)
	if a.QueuedInputCount() != 1 {
		t.Fatalf("queued = %d, want 1", a.QueuedInputCount())
	}
	items := a.DrainQueuedInputs()
	if !strings.Contains(items[0].Text, "run the checks") || items[0].Tag != "scheduled-task" {
		t.Fatalf("notification = %+v", items[0])
	}

	// The same minute never double-fires; the next minute does.
	a.fireDueCron(now.Add(10 * time.Second))
	if a.QueuedInputCount() != 0 {
		t.Fatal("double fire within the same minute")
	}
	a.fireDueCron(now.Add(time.Minute))
	if a.QueuedInputCount() != 1 {
		t.Fatal("next minute should fire again")
	}
	a.DrainQueuedInputs()

	if !a.deleteCron(t.Context(), id) {
		t.Fatal("delete failed")
	}
	a.fireDueCron(now.Add(2 * time.Minute))
	if a.QueuedInputCount() != 0 {
		t.Fatal("deleted entry fired")
	}
}

func TestScheduleCronToolLifecycle(t *testing.T) {
	a := newCronAgent(t)
	tool, ok := a.toolByName["ScheduleCron"]
	if !ok {
		t.Fatal("ScheduleCron tool not registered")
	}

	res, err := tool.Invoke(t.Context(), map[string]any{
		"prompt": "run the checks", "cron": "*/10 * * * *", "name": "checks",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "Scheduled cron-1") {
		t.Fatalf("create = %q", res.Content)
	}

	res, err = tool.Invoke(t.Context(), map[string]any{"list": true})
	if err != nil || !strings.Contains(res.Content, "cron-1") || !strings.Contains(res.Content, "checks") {
		t.Fatalf("list = %q err=%v", res.Content, err)
	}

	res, _ = tool.Invoke(t.Context(), map[string]any{"delete_id": "cron-1"})
	if !strings.Contains(res.Content, "Deleted") {
		t.Fatalf("delete = %q", res.Content)
	}
	res, _ = tool.Invoke(t.Context(), map[string]any{"delete_id": "cron-1"})
	if !strings.Contains(res.Content, "Error") {
		t.Fatalf("second delete = %q", res.Content)
	}

	res, _ = tool.Invoke(t.Context(), map[string]any{"prompt": "x", "cron": "bogus"})
	if !strings.Contains(res.Content, "Error") {
		t.Fatalf("bad spec = %q", res.Content)
	}
}

// TestCronVixieDayFieldRule pins the Vixie cron semantics: when day-of-month
// and day-of-week are both restricted, a time matches when EITHER field
// matches (OR); with only one restricted, both must match (AND).
func TestCronVixieDayFieldRule(t *testing.T) {
	// 2026-08-13 is a Thursday, 2026-08-14 a Friday, 2026-08-15 a Saturday.
	m, err := parseCron("0 0 13 * 5") // 13th OR Friday
	if err != nil {
		t.Fatal(err)
	}
	friday14th := time.Date(2026, 8, 14, 0, 0, 0, 0, time.Local)
	if !m.matches(friday14th) {
		t.Error("Friday the 14th must match via day-of-week despite dom=13")
	}
	thursday13th := time.Date(2026, 8, 13, 0, 0, 0, 0, time.Local)
	if !m.matches(thursday13th) {
		t.Error("the 13th must match via day-of-month despite dow=Friday")
	}
	saturday15th := time.Date(2026, 8, 15, 0, 0, 0, 0, time.Local)
	if m.matches(saturday15th) {
		t.Error("neither the 13th nor a Friday: must not match")
	}

	// Single restricted day field keeps AND semantics.
	m, err = parseCron("0 0 13 * *")
	if err != nil {
		t.Fatal(err)
	}
	if m.matches(friday14th) {
		t.Error("dom-only spec must require day 13")
	}
}

// TestCronRangeRejectsTrailingGarbage ensures "1-5x" style ranges fail
// instead of silently parsing as 1-5.
func TestCronRangeRejectsTrailingGarbage(t *testing.T) {
	for _, spec := range []string{"0 9 * * 1-5x", "0 9 1-3x * *", "0 9 1x-3 * *"} {
		if _, err := parseCron(spec); err == nil {
			t.Errorf("parseCron(%q) should fail", spec)
		}
	}
}

// TestCronSchedulerSessionScoped pins that a firing entry carries the session
// it was created for: the sink's first argument is the entry's captured
// session, so a multi-session host delivers each notification to its owner.
func TestCronSchedulerSessionScoped(t *testing.T) {
	var mu sync.Mutex
	fired := [][3]string{}
	s := newCronScheduler(func(session, tag, text string) {
		mu.Lock()
		fired = append(fired, [3]string{session, tag, text})
		mu.Unlock()
	})
	if _, err := s.Add("s1", "check", "run the checks", "* * * * *"); err != nil {
		t.Fatal(err)
	}
	s.fireDue(time.Date(2026, 8, 16, 12, 0, 0, 0, time.Local))
	mu.Lock()
	defer mu.Unlock()
	if len(fired) != 1 {
		t.Fatalf("fired %d notifications, want 1: %v", len(fired), fired)
	}
	if got := fired[0]; got[0] != "s1" || got[1] != "scheduled-task" || !strings.Contains(got[2], "run the checks") {
		t.Fatalf("sink got (%q, %q, %q), want (s1, scheduled-task, …run the checks…)", got[0], got[1], got[2])
	}
}

// TestCronSchedulerStandalone pins the component contract: the scheduler
// works without an Agent, delivering through the injected sink only.
func TestCronSchedulerStandalone(t *testing.T) {
	var mu sync.Mutex
	fired := []string{}
	s := newCronScheduler(func(session, tag, text string) {
		mu.Lock()
		fired = append(fired, tag+"|"+text)
		mu.Unlock()
	})
	id, err := s.Add("", "check", "run the checks", "* * * * *")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.Local)
	s.fireDue(now)
	s.fireDue(now.Add(30 * time.Second)) // same minute: suppressed
	s.fireDue(now.Add(time.Minute))      // next minute: fires again
	if !s.Delete(id, "") || s.Delete(id, "") {
		t.Fatal("delete should succeed exactly once")
	}
	s.fireDue(now.Add(2 * time.Minute))
	mu.Lock()
	defer mu.Unlock()
	if len(fired) != 2 {
		t.Fatalf("fired %d notifications, want 2 (minute de-dup + post-delete silence): %v", len(fired), fired)
	}
}

// TestScheduleCronListDeleteSessionScoped mirrors the queue's
// TestQueueNotificationForOtherSessionStaysQueuedMidRun: one Agent serves
// many web sessions, so session B's model must not see or delete session
// A's entries — a foreign id reads as not-found and the entry survives.
// Unscoped ("") entries — the TUI/REPL single-session case — stay visible
// and deletable for every session, and the unscoped caller sees everything.
func TestScheduleCronListDeleteSessionScoped(t *testing.T) {
	a := newCronAgent(t)
	tool := a.toolByName["ScheduleCron"]
	ctxA := tracing.WithRun(t.Context(), "run-a", "sess-a")
	ctxB := tracing.WithRun(t.Context(), "run-b", "sess-b")

	idA, err := a.addCron(ctxA, "a-checks", "a's checks", "*/10 * * * *")
	if err != nil {
		t.Fatal(err)
	}
	idB, err := a.addCron(ctxB, "b-checks", "b's checks", "*/10 * * * *")
	if err != nil {
		t.Fatal(err)
	}
	idU, err := a.addCron(t.Context(), "shared", "unscoped prompt", "* * * * *") // session ""
	if err != nil {
		t.Fatal(err)
	}

	// B's list: its own entry plus the unscoped one — never A's.
	res, err := tool.Invoke(ctxB, map[string]any{"list": true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Content, "a-checks") {
		t.Fatalf("session B sees session A's entry: %q", res.Content)
	}
	if !strings.Contains(res.Content, "b-checks") || !strings.Contains(res.Content, "shared") {
		t.Fatalf("session B's list missing its own/unscoped entries: %q", res.Content)
	}

	// B cannot delete A's entry: the id reads as not-found and the entry
	// survives for A.
	res, _ = tool.Invoke(ctxB, map[string]any{"delete_id": idA})
	if !strings.Contains(res.Content, "Error") {
		t.Fatalf("session B deleting session A's entry = %q, want not-found error", res.Content)
	}
	if entries := a.cron.List(""); len(entries) != 3 {
		t.Fatalf("%d entries after the cross-session delete attempt, want 3", len(entries))
	}

	// B deletes its own; A's list still shows A's own plus the unscoped one.
	res, _ = tool.Invoke(ctxB, map[string]any{"delete_id": idB})
	if !strings.Contains(res.Content, "Deleted "+idB) {
		t.Fatalf("session B deleting its own entry = %q", res.Content)
	}
	res, _ = tool.Invoke(ctxA, map[string]any{"list": true})
	if !strings.Contains(res.Content, "a-checks") || !strings.Contains(res.Content, "shared") ||
		strings.Contains(res.Content, "b-checks") {
		t.Fatalf("session A's list wrong after B's delete: %q", res.Content)
	}

	// The unscoped caller (TUI/REPL) still sees and deletes everything.
	res, _ = tool.Invoke(t.Context(), map[string]any{"list": true})
	if !strings.Contains(res.Content, "a-checks") || !strings.Contains(res.Content, "shared") {
		t.Fatalf("unscoped list missing entries: %q", res.Content)
	}
	res, _ = tool.Invoke(t.Context(), map[string]any{"delete_id": idA})
	if !strings.Contains(res.Content, "Deleted "+idA) {
		t.Fatalf("unscoped delete of session A's entry = %q", res.Content)
	}
	res, _ = tool.Invoke(t.Context(), map[string]any{"delete_id": idU})
	if !strings.Contains(res.Content, "Deleted "+idU) {
		t.Fatalf("unscoped delete of the unscoped entry = %q", res.Content)
	}
	if entries := a.cron.List(""); len(entries) != 0 {
		t.Fatalf("%d entries at the end, want 0", len(entries))
	}
}
