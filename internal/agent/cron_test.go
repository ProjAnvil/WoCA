package agent

import (
	"fmt"
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

// TestCronMatcherNext pins the next-fire minute scan (AC7) against a fixed
// clock: strictly-after semantics, local-time wall matching, and no-match
// within the horizon for impossible dates.
func TestCronMatcherNext(t *testing.T) {
	// Anchors from TestCronVixieDayFieldRule: 2026-08-14 is a Friday,
	// 2026-08-16 a Sunday, 2026-08-17 a Monday.
	cases := []struct {
		spec    string
		after   time.Time
		horizon time.Duration
		want    time.Time
		ok      bool
	}{
		// */15: the next quarter hour, also when `after` is mid-minute.
		{"*/15 * * * *", time.Date(2026, 8, 14, 10, 7, 0, 0, time.Local), 24 * time.Hour, time.Date(2026, 8, 14, 10, 15, 0, 0, time.Local), true},
		// Strictly after: 10:30:12 → 10:45, not 10:30.
		{"*/15 * * * *", time.Date(2026, 8, 14, 10, 30, 12, 0, time.Local), 24 * time.Hour, time.Date(2026, 8, 14, 10, 45, 0, 0, time.Local), true},
		// An exact matching minute counts only for later `after`s.
		{"*/15 * * * *", time.Date(2026, 8, 14, 10, 30, 0, 0, time.Local), 24 * time.Hour, time.Date(2026, 8, 14, 10, 45, 0, 0, time.Local), true},
		// Weekdays at 09:00: after Friday 10:00 the next match is Monday.
		{"0 9 * * 1-5", time.Date(2026, 8, 14, 10, 0, 0, 0, time.Local), 7 * 24 * time.Hour, time.Date(2026, 8, 17, 9, 0, 0, 0, time.Local), true},
		// Before the time on a weekday: same day.
		{"0 9 * * 1-5", time.Date(2026, 8, 14, 8, 0, 0, 0, time.Local), 7 * 24 * time.Hour, time.Date(2026, 8, 14, 9, 0, 0, 0, time.Local), true},
		// Sunday never matches a weekday spec.
		{"0 9 * * 1-5", time.Date(2026, 8, 16, 10, 0, 0, 0, time.Local), 7 * 24 * time.Hour, time.Date(2026, 8, 17, 9, 0, 0, 0, time.Local), true},
		// February 31st never exists: no next fire, whatever the horizon.
		{"0 0 31 2 *", time.Date(2026, 8, 14, 10, 0, 0, 0, time.Local), 400 * 24 * time.Hour, time.Time{}, false},
		// A real match beyond the horizon reports none.
		{"0 9 * * 1-5", time.Date(2026, 8, 14, 10, 0, 0, 0, time.Local), 24 * time.Hour, time.Time{}, false},
	}
	for _, tc := range cases {
		m, err := parseCron(tc.spec)
		if err != nil {
			t.Fatalf("parseCron(%q): %v", tc.spec, err)
		}
		got, ok := m.next(tc.after, tc.horizon)
		if ok != tc.ok || (ok && !got.Equal(tc.want)) {
			t.Errorf("next(%q, %v, %v) = %v,%v want %v,%v", tc.spec, tc.after, tc.horizon, got, ok, tc.want, tc.ok)
		}
	}
}

// TestCronInfoForSessionScopedTimestampsAndOrder covers InfoFor: the §4.1
// visibility rule, numeric id order (cron-2 before cron-10), created_at /
// last_fired_at facts and next_fire_at read from the per-entry cache (F5:
// computed at Add — unfired entries report a value near the test's real
// start time, independent of the caller's clock — recomputed on fire;
// `0 0 31 2 *` caches and renders a zero NextFireAt).
func TestCronInfoForSessionScopedTimestampsAndOrder(t *testing.T) {
	s := newCronScheduler(func(session, tag, text string) {})
	t0 := time.Now()
	if _, err := s.Add("sess-A", "a", "a's prompt", "*/10 * * * *"); err != nil {
		t.Fatal(err)
	}
	// Ten more entries so numeric order shows: cron-2 must precede cron-10.
	for i := range 10 {
		if _, err := s.Add("sess-B", "b", "b's prompt", "* * * * *"); err != nil {
			t.Fatal(i, err)
		}
	}
	if _, err := s.Add("sess-A", "impossible", "never fires", "0 0 31 2 *"); err != nil {
		t.Fatal(err)
	}
	uID, err := s.Add("", "shared", "unscoped prompt", "0 9 * * 1-5") // owner ""
	if err != nil {
		t.Fatal(err)
	}

	// Fire the every-minute entries once at a fixed minute that the */10
	// entry does not match (:31): last_fired_at records that minute, and
	// cron-1 stays unfired → zero.
	firedAt := time.Date(2026, 8, 14, 10, 31, 0, 0, time.Local)
	s.fireDue(firedAt)

	bInfos := s.InfoFor("sess-B")
	if len(bInfos) != 11 {
		t.Fatalf("viewer B sees %d entries, want 11 (own + unscoped)", len(bInfos))
	}
	if bInfos[0].ID != "cron-2" || bInfos[8].ID != "cron-10" || bInfos[9].ID != "cron-11" || bInfos[10].ID != uID {
		t.Fatalf("viewer B order wrong: %s…%s/%s, %s", bInfos[0].ID, bInfos[8].ID, bInfos[9].ID, bInfos[10].ID)
	}
	first := bInfos[0]
	if first.Session != "sess-B" || first.Prompt != "b's prompt" || first.Spec != "* * * * *" {
		t.Fatalf("first info = %+v", first)
	}
	if first.CreatedAt.IsZero() {
		t.Fatal("CreatedAt not recorded")
	}
	if !first.LastFiredAt.Equal(firedAt) {
		t.Fatalf("LastFiredAt = %v, want %v", first.LastFiredAt, firedAt)
	}
	// Recomputed on fire (F5): the minute after the fired one.
	if want := time.Date(2026, 8, 14, 10, 32, 0, 0, time.Local); !first.NextFireAt.Equal(want) {
		t.Fatalf("NextFireAt = %v, want %v", first.NextFireAt, want)
	}

	// Viewer A: its own two entries plus the unscoped one; the impossible
	// spec renders a zero NextFireAt and cron-1 stays never-fired.
	aInfos := s.InfoFor("sess-A")
	if len(aInfos) != 3 {
		t.Fatalf("viewer A sees %d entries, want 3", len(aInfos))
	}
	if aInfos[0].ID != "cron-1" || !aInfos[0].LastFiredAt.IsZero() {
		t.Fatalf("cron-1 info = %+v, want never fired", aInfos[0])
	}
	// Computed at Add (F5): a */10 boundary within the ten minutes after the
	// test started.
	if nf := aInfos[0].NextFireAt; nf.Before(t0) || !nf.Before(t0.Add(11*time.Minute)) || nf.Minute()%10 != 0 {
		t.Fatalf("cron-1 cached NextFireAt = %v, want a */10 boundary within (Add, Add+10min]", nf)
	}
	if aInfos[1].ID != "cron-12" || !aInfos[1].NextFireAt.IsZero() {
		t.Fatalf("impossible spec info = %+v, want zero NextFireAt", aInfos[1])
	}
	// The unscoped entry, likewise cached at Add: a 09:00 weekday minute
	// within days of the test start.
	if nf := aInfos[2].NextFireAt; nf.IsZero() || nf.Hour() != 9 || nf.Minute() != 0 || nf.Weekday() == time.Saturday || nf.Weekday() == time.Sunday || nf.Before(t0) || !nf.Before(t0.Add(8*24*time.Hour)) {
		t.Fatalf("unscoped cached NextFireAt = %v, want a 09:00 weekday within (Add, Add+8d]", nf)
	}

	// The unscoped viewer sees everything: 13 entries, numeric order.
	if all := s.InfoFor(""); len(all) != 13 || all[0].ID != "cron-1" || all[11].ID != "cron-12" {
		t.Fatalf("unscoped viewer sees %d entries, first=%s", len(all), all[0].ID)
	}
}

// TestCronInfoForConcurrentWithFire locks InfoFor's snapshot against the
// mutating paths (fireDue writes lastFired and the next-fire cache,
// Add/Delete reshape the slice): parallel readers and writers must stay
// race-detector-clean.
func TestCronInfoForConcurrentWithFire(t *testing.T) {
	s := newCronScheduler(func(session, tag, text string) {})
	if _, err := s.Add("sess-A", "a", "p", "* * * * *"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add("sess-B", "b", "p", "*/2 * * * *"); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 8, 14, 10, 0, 0, 0, time.Local)
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for i := range 25 {
			s.fireDue(base.Add(time.Duration(i) * time.Minute))
		}
	}()
	go func() {
		defer wg.Done()
		for range 25 {
			if infos := s.InfoFor("sess-A"); len(infos) != 1 {
				t.Errorf("sess-A sees %d entries, want 1", len(infos))
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := range 25 {
			if i%2 == 0 {
				if _, err := s.Add("sess-B", "extra", "p", "0 9 * * 1-5"); err != nil {
					t.Error(err)
					return
				}
			} else {
				s.Delete(fmt.Sprintf("cron-%d", 3+i/2), "sess-B")
			}
		}
	}()
	wg.Wait()
}

// TestCronSetOnChange pins the change-hook contract: fires on Add, on an
// actual Delete (never for a foreign id that reads as not-found), and on
// every fire — always with the owning session id.
func TestCronSetOnChange(t *testing.T) {
	var mu sync.Mutex
	var fired []string
	s := newCronScheduler(func(session, tag, text string) {})
	s.SetOnChange(func(session string) {
		mu.Lock()
		fired = append(fired, session)
		mu.Unlock()
	})

	idA, err := s.Add("sess-A", "a", "a's prompt", "* * * * *")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add("sess-B", "b", "b's prompt", "* * * * *"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	addFired := strings.Join(fired, ",")
	mu.Unlock()
	if addFired != "sess-A,sess-B" {
		t.Fatalf("Add fired %q, want sess-A,sess-B", addFired)
	}

	// A fire signals the fired entry's owner.
	s.fireDue(time.Date(2026, 8, 14, 10, 30, 0, 0, time.Local))
	mu.Lock()
	afterFire := strings.Join(fired, ",")
	mu.Unlock()
	if afterFire != "sess-A,sess-B,sess-A,sess-B" {
		t.Fatalf("fire fired %q", afterFire)
	}

	// A foreign delete reads as not-found and fires nothing.
	if s.Delete(idA, "sess-B") {
		t.Fatal("foreign delete succeeded")
	}
	mu.Lock()
	afterForeign := strings.Join(fired, ",")
	mu.Unlock()
	if afterForeign != afterFire {
		t.Fatalf("foreign delete fired a change: %q", afterForeign)
	}

	// The owner's delete fires once.
	if !s.Delete(idA, "sess-A") {
		t.Fatal("owner delete failed")
	}
	mu.Lock()
	defer mu.Unlock()
	if got := strings.Join(fired, ","); got != afterFire+",sess-A" {
		t.Fatalf("changes = %q, want one more sess-A", got)
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

// TestCronNextFireAtCached pins F5: next_fire_at is computed once per entry
// (at Add), recomputed after each fire, and read from the cache by
// snapshots — the caller's clock is irrelevant — and an impossible spec pays
// its single full-horizon scan at Add, then caches the zero forever.
func TestCronNextFireAtCached(t *testing.T) {
	s := newCronScheduler(func(session, tag, text string) {})

	// Computed at Add: a */10 entry's cached next fire lies within the ten
	// minutes after the Add, no matter when the snapshot is taken.
	pre := time.Now()
	if _, err := s.Add("sess-A", "a", "p", "*/10 * * * *"); err != nil {
		t.Fatal(err)
	}
	infos := s.InfoFor("sess-A")
	nf := infos[0].NextFireAt
	if nf.IsZero() || nf.Before(pre) || !nf.Before(pre.Add(11*time.Minute)) {
		t.Fatalf("cached next fire = %v, want within (Add, Add+10min]", nf)
	}
	if nf.Minute()%10 != 0 {
		t.Fatalf("cached next fire %v is not a */10 boundary", nf)
	}

	// Recomputed after each fire: firing at a fixed matching minute pins the
	// cache to the following minute.
	s.fireDue(time.Date(2026, 8, 14, 10, 40, 0, 0, time.Local))
	infos = s.InfoFor("sess-A")
	if want := time.Date(2026, 8, 14, 10, 50, 0, 0, time.Local); !infos[0].NextFireAt.Equal(want) {
		t.Fatalf("post-fire cached next = %v, want %v", infos[0].NextFireAt, want)
	}
	if !infos[0].LastFiredAt.Equal(time.Date(2026, 8, 14, 10, 40, 0, 0, time.Local)) {
		t.Fatalf("last fired = %v", infos[0].LastFiredAt)
	}

	// An impossible spec scans its full horizon once (at Add) and caches the
	// zero: hundreds of snapshots stay cheap instead of rescanning ~45ms
	// each (300 uncached scans would take well over ten seconds).
	if _, err := s.Add("sess-A", "imp", "p", "0 0 31 2 *"); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	for range 300 {
		if infos = s.InfoFor("sess-A"); len(infos) != 2 {
			t.Fatalf("sess-A sees %d entries, want 2", len(infos))
		}
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("300 snapshots took %v — next-fire is not cached (an uncached full-horizon rescan per snapshot would take ~13s)", d)
	}
	if !infos[1].NextFireAt.IsZero() {
		t.Fatalf("impossible spec cached next = %v, want zero", infos[1].NextFireAt)
	}

	// Delete drops the cache with the entry; a re-added entry recomputes.
	if !s.Delete(infos[1].ID, "sess-A") {
		t.Fatal("delete failed")
	}
	if got := s.InfoFor("sess-A"); len(got) != 1 {
		t.Fatalf("post-delete entries = %d, want 1", len(got))
	}
}
