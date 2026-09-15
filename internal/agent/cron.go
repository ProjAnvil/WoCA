package agent

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	coretools "github.com/projanvil/langchain-golang/core/tools"

	"github.com/projanvil/woca/internal/tools"
)

// ScheduleCron: the model can schedule prompts that fire on a cron schedule
// for the lifetime of the session. A fired entry injects a <scheduled-task>
// notification through the mid-turn queue, so the agent acts on it in the
// next tool round (or the next run's first round when idle) — the same
// delivery path as background-shell completions. Entries die with the
// session (nothing is persisted).

// cronEntry is one scheduled prompt.
type cronEntry struct {
	ID        string
	Name      string
	Prompt    string
	Spec      string
	matcher   cronMatcher
	lastFired time.Time // minute granularity, prevents double fire
}

// notificationSink is the single capability the scheduler needs from its
// host: delivering a due prompt into the session's input queue.
type notificationSink func(tag, text string)

// cronScheduler is the session-scoped scheduled-prompt loop, extracted from
// the Agent so the scheduling logic (parse / match / fire / ticker) is
// testable without an agent. The host injects the delivery sink at
// construction; everything else is self-contained.
type cronScheduler struct {
	notify  notificationSink
	mu      sync.Mutex
	next    int
	entries []*cronEntry
	cancel  context.CancelFunc
}

func newCronScheduler(notify notificationSink) *cronScheduler {
	return &cronScheduler{notify: notify}
}

// Start starts the ticker loop (idempotent).
func (s *cronScheduler) Start() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	go func() {
		ticker := time.NewTicker(20 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.fireDue(time.Now())
			}
		}
	}()
}

// Stop halts the scheduler (tests / teardown).
func (s *cronScheduler) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
}

// Add registers a scheduled prompt and returns its ID.
func (s *cronScheduler) Add(name, prompt, spec string) (string, error) {
	m, err := parseCron(spec)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	id := fmt.Sprintf("cron-%d", s.next)
	s.entries = append(s.entries, &cronEntry{
		ID: id, Name: name, Prompt: prompt, Spec: spec, matcher: m,
	})
	return id, nil
}

// Delete removes an entry by ID.
func (s *cronScheduler) Delete(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, e := range s.entries {
		if e.ID == id {
			s.entries = append(s.entries[:i], s.entries[i+1:]...)
			return true
		}
	}
	return false
}

// List snapshots the entries.
func (s *cronScheduler) List() []cronEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]cronEntry, 0, len(s.entries))
	for _, e := range s.entries {
		out = append(out, cronEntry{ID: e.ID, Name: e.Name, Prompt: e.Prompt, Spec: e.Spec})
	}
	slices.SortFunc(out, func(a, b cronEntry) int { return cmp.Compare(a.ID, b.ID) })
	return out
}

// fireDue fires every entry whose schedule matches now's minute. Split out
// from the ticker loop so tests can drive it deterministically.
func (s *cronScheduler) fireDue(now time.Time) {
	s.mu.Lock()
	due := make([]*cronEntry, 0, len(s.entries))
	for _, e := range s.entries {
		minute := now.Truncate(time.Minute)
		if e.matcher.matches(now) && !e.lastFired.Equal(minute) {
			e.lastFired = minute
			due = append(due, e)
		}
	}
	s.mu.Unlock()
	for _, e := range due {
		s.notify("scheduled-task",
			fmt.Sprintf("Scheduled task %s (%s) is due now:\n%s", e.displayName(), e.Spec, e.Prompt))
	}
}

// Agent delegates: the tool wiring, Close() teardown and the tests keep
// their entry points; the mechanics live on the scheduler.
func (a *Agent) addCron(name, prompt, spec string) (string, error) {
	return a.cron.Add(name, prompt, spec)
}

func (a *Agent) deleteCron(id string) bool { return a.cron.Delete(id) }

func (a *Agent) listCron() []cronEntry { return a.cron.List() }

func (a *Agent) startCron() { a.cron.Start() }

func (a *Agent) stopCron() { a.cron.Stop() }

func (a *Agent) fireDueCron(now time.Time) { a.cron.fireDue(now) }

func (e *cronEntry) displayName() string {
	if e.Name != "" {
		return e.Name
	}
	return e.ID
}

// cronMatcher matches a 5-field cron expression against a time (minute
// granularity, local time).
type cronMatcher struct {
	fields [5]map[int]bool
	// bothDayFields tracks the Vixie rule: when day-of-month and day-of-week
	// are both restricted (neither is "*"), a time matches when EITHER field
	// matches instead of both (see matches).
	bothDayFields bool
}

func parseCron(spec string) (cronMatcher, error) {
	fields := strings.Fields(spec)
	if len(fields) != 5 {
		return cronMatcher{}, fmt.Errorf("cron: expected 5 fields (min hour dom month dow), got %d in %q", len(fields), spec)
	}
	bounds := [][2]int{{0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 6}}
	var m cronMatcher
	for i, f := range fields {
		set, err := parseCronField(f, bounds[i][0], bounds[i][1])
		if err != nil {
			return cronMatcher{}, err
		}
		m.fields[i] = set
	}
	m.bothDayFields = fields[2] != "*" && fields[4] != "*"
	return m, nil
}

func parseCronField(field string, min, max int) (map[int]bool, error) {
	set := map[int]bool{}
	for part := range strings.SplitSeq(field, ",") {
		step := 1
		if idx := strings.IndexByte(part, '/'); idx >= 0 {
			n, err := strconv.Atoi(part[idx+1:])
			if err != nil || n < 1 {
				return nil, fmt.Errorf("cron: invalid step in %q", field)
			}
			step = n
			part = part[:idx]
		}
		lo, hi := min, max
		switch {
		case part == "*" || part == "":
		case strings.Contains(part, "-"):
			loS, hiS, _ := strings.Cut(part, "-")
			a, errA := strconv.Atoi(loS)
			b, errB := strconv.Atoi(hiS)
			if errA != nil || errB != nil {
				return nil, fmt.Errorf("cron: invalid range %q", part)
			}
			lo, hi = a, b
		default:
			n, err := strconv.Atoi(part)
			if err != nil {
				return nil, fmt.Errorf("cron: invalid value %q", part)
			}
			lo, hi = n, n
		}
		if lo < min || hi > max || lo > hi {
			return nil, fmt.Errorf("cron: %q out of range %d-%d", field, min, max)
		}
		for v := lo; v <= hi; v += step {
			set[v] = true
		}
	}
	if len(set) == 0 {
		return nil, fmt.Errorf("cron: empty field %q", field)
	}
	return set, nil
}

func (m cronMatcher) matches(t time.Time) bool {
	if !m.fields[0][t.Minute()] || !m.fields[1][t.Hour()] || !m.fields[3][int(t.Month())] {
		return false
	}
	// Vixie cron: with both day fields restricted, either one matching is
	// enough; otherwise both must match.
	if m.bothDayFields {
		return m.fields[2][t.Day()] || m.fields[4][int(t.Weekday())]
	}
	return m.fields[2][t.Day()] && m.fields[4][int(t.Weekday())]
}

const scheduleCronDescription = `Schedule a recurring or one-shot scheduled prompt using a cron expression (5 fields: minute hour day-of-month month day-of-week, local time). When an entry is due, its prompt is delivered to you as a <scheduled-task> notification — act on it like new user input. Entries live for the current session only.

Actions:
- create: set prompt + cron (and optionally name)
- list: set list=true
- delete: set delete_id to an entry ID from list`

type scheduleCronArgs struct {
	Prompt   string `json:"prompt,omitempty"`
	Cron     string `json:"cron,omitempty"`
	Name     string `json:"name,omitempty"`
	List     bool   `json:"list,omitempty"`
	DeleteID string `json:"delete_id,omitempty"`
}

// scheduleCronTool builds the ScheduleCron tool.
func (a *Agent) scheduleCronTool() []coretools.Tool {
	t := tools.MustTool(coretools.FromFunc("ScheduleCron", scheduleCronDescription,
		func(ctx context.Context, args scheduleCronArgs) (coretools.Result, error) {
			if args.DeleteID != "" {
				if a.deleteCron(args.DeleteID) {
					return coretools.Result{Content: "Deleted " + args.DeleteID}, nil
				}
				return coretools.Result{Content: "Error: no scheduled entry " + args.DeleteID}, nil
			}
			if args.List {
				entries := a.listCron()
				if len(entries) == 0 {
					return coretools.Result{Content: "(no scheduled entries)"}, nil
				}
				var b strings.Builder
				for _, e := range entries {
					fmt.Fprintf(&b, "%s  %-24s %s\n", e.ID, e.Spec, previewCron(e))
				}
				return coretools.Result{Content: b.String()}, nil
			}
			if args.Prompt == "" || args.Cron == "" {
				return coretools.Result{Content: "Error: creating an entry requires prompt and cron"}, nil
			}
			id, err := a.addCron(args.Name, args.Prompt, args.Cron)
			if err != nil {
				return coretools.Result{Content: "Error: " + err.Error()}, nil
			}
			a.startCron()
			return coretools.Result{Content: fmt.Sprintf("Scheduled %s (%s). It will fire as a <scheduled-task> notification when due. Session-only; delete with delete_id=%s.", id, args.Cron, id)}, nil
		}))
	return []coretools.Tool{t}
}

func previewCron(e cronEntry) string {
	if e.Name != "" {
		return e.Name + ": " + previewText(e.Prompt, 40)
	}
	return previewText(e.Prompt, 60)
}

func previewText(s string, max int) string {
	s = strings.TrimSpace(s)
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		s = s[:idx]
	}
	runes := []rune(s)
	if len(runes) > max {
		return string(runes[:max]) + "…"
	}
	return s
}
