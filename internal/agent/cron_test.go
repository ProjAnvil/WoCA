package agent

import (
	"context"
	"strings"
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

	res, err := tool.Invoke(context.Background(), map[string]any{
		"prompt": "run the checks", "cron": "*/10 * * * *", "name": "checks",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "Scheduled cron-1") {
		t.Fatalf("create = %q", res.Content)
	}

	res, err = tool.Invoke(context.Background(), map[string]any{"list": true})
	if err != nil || !strings.Contains(res.Content, "cron-1") || !strings.Contains(res.Content, "checks") {
		t.Fatalf("list = %q err=%v", res.Content, err)
	}

	res, _ = tool.Invoke(context.Background(), map[string]any{"delete_id": "cron-1"})
	if !strings.Contains(res.Content, "Deleted") {
		t.Fatalf("delete = %q", res.Content)
	}
	res, _ = tool.Invoke(context.Background(), map[string]any{"delete_id": "cron-1"})
	if !strings.Contains(res.Content, "Error") {
		t.Fatalf("second delete = %q", res.Content)
	}

	res, _ = tool.Invoke(context.Background(), map[string]any{"prompt": "x", "cron": "bogus"})
	if !strings.Contains(res.Content, "Error") {
		t.Fatalf("bad spec = %q", res.Content)
	}
}
