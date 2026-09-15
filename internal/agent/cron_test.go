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
	id, err := a.addCron("check", "run the checks", "* * * * *")
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

	if !a.deleteCron(id) {
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

// TestCronSchedulerStandalone pins the component contract: the scheduler
// works without an Agent, delivering through the injected sink only.
func TestCronSchedulerStandalone(t *testing.T) {
	var mu sync.Mutex
	fired := []string{}
	s := newCronScheduler(func(tag, text string) {
		mu.Lock()
		fired = append(fired, tag+"|"+text)
		mu.Unlock()
	})
	id, err := s.Add("check", "run the checks", "* * * * *")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.Local)
	s.fireDue(now)
	s.fireDue(now.Add(30 * time.Second)) // same minute: suppressed
	s.fireDue(now.Add(time.Minute))      // next minute: fires again
	if !s.Delete(id) || s.Delete(id) {
		t.Fatal("delete should succeed exactly once")
	}
	s.fireDue(now.Add(2 * time.Minute))
	mu.Lock()
	defer mu.Unlock()
	if len(fired) != 2 {
		t.Fatalf("fired %d notifications, want 2 (minute de-dup + post-delete silence): %v", len(fired), fired)
	}
}
