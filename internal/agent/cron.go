package agent

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	coretools "github.com/projanvil/langchain-golang/core/tools"

	"github.com/projanvil/woca/internal/tools"
	"github.com/projanvil/woca/internal/tracing"
)

// ScheduleCron: the model can schedule prompts that fire on a cron schedule
// for the lifetime of the session. A fired entry injects a <scheduled-task>
// notification through the mid-turn queue, so the agent acts on it in the
// next tool round (or the next run's first round when idle) — the same
// delivery path as background-shell completions. Entries die with the
// session (nothing is persisted).

// cronEntry is one scheduled prompt, scoped to the session that created it.
type cronEntry struct {
	ID        string
	Session   string
	Name      string
	Prompt    string
	Spec      string
	matcher   cronMatcher
	createdAt time.Time
	lastFired time.Time // minute granularity, prevents double fire
}

// CronInfo is a snapshot of one scheduled entry: identity plus the three
// timestamps. LastFiredAt is zero when the entry never fired; NextFireAt is
// zero when no matching minute exists ahead (e.g. "0 0 31 2 *": February
// 31st never exists).
type CronInfo struct {
	ID, Session, Name, Spec, Prompt    string
	CreatedAt, LastFiredAt, NextFireAt time.Time
}

// notificationSink is the single capability the scheduler needs from its
// host: delivering a due prompt into the creating session's input queue.
type notificationSink func(session, tag, text string)

// cronScheduler is the session-scoped scheduled-prompt loop, extracted from
// the Agent so the scheduling logic (parse / match / fire / ticker) is
// testable without an agent. The host injects the delivery sink at
// construction; everything else is self-contained.
type cronScheduler struct {
	notify   notificationSink
	mu       sync.Mutex
	next     int
	entries  []*cronEntry
	onChange func(session string)
	cancel   context.CancelFunc
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

// Add registers a scheduled prompt owned by session and returns its ID.
func (s *cronScheduler) Add(session, name, prompt, spec string) (string, error) {
	m, err := parseCron(spec)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	onChange := s.onChange
	s.next++
	id := fmt.Sprintf("cron-%d", s.next)
	s.entries = append(s.entries, &cronEntry{
		ID: id, Session: session, Name: name, Prompt: prompt, Spec: spec, matcher: m,
		createdAt: time.Now(),
	})
	s.mu.Unlock()
	if onChange != nil {
		onChange(session)
	}
	return id, nil
}

// SetOnChange installs the change callback the activity seam subscribes to.
// It fires synchronously on Add, on Delete and on every fire, with the
// owning session id; it must not block.
func (s *cronScheduler) SetOnChange(fn func(session string)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onChange = fn
}

// Delete removes an entry by ID when the calling session may touch it:
// session "" (the TUI/REPL single-session case) deletes any entry, and a
// scoped session deletes its own plus unscoped ("") entries — the same
// scope rule as queue.drain. An entry that exists but belongs to another
// session is returned as not-found, never removed.
func (s *cronScheduler) Delete(id, session string) bool {
	s.mu.Lock()
	onChange := s.onChange
	removed := ""
	for i, e := range s.entries {
		if e.ID != id {
			continue
		}
		if !tracing.VisibleTo(e.Session, session) {
			s.mu.Unlock()
			return false // exists, but another session owns it
		}
		removed = e.Session
		s.entries = append(s.entries[:i], s.entries[i+1:]...)
		s.mu.Unlock()
		if onChange != nil {
			onChange(removed)
		}
		return true
	}
	s.mu.Unlock()
	return false
}

// List snapshots the entries a run in session sees: everything when
// unscoped ("" — the single-session surfaces see the whole list),
// otherwise the unscoped entries plus the session's own, in ID order. The
// entries slice is append-only with monotonic cron-N ids, so its order IS
// the numeric id order (cron-2 before cron-10).
func (s *cronScheduler) List(session string) []cronEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]cronEntry, 0, len(s.entries))
	for _, e := range s.entries {
		if !tracing.VisibleTo(e.Session, session) {
			continue
		}
		out = append(out, cronEntry{ID: e.ID, Session: e.Session, Name: e.Name, Prompt: e.Prompt, Spec: e.Spec})
	}
	return out
}

// InfoFor snapshots the entries a run in session can see, in numeric id
// order, with their timestamps and the next fire computed from now. The
// snapshot copies every field under the scheduler lock (lastFired mutates on
// fire); the pure next-fire scan then runs outside it.
func (s *cronScheduler) InfoFor(session string, now time.Time) []CronInfo {
	s.mu.Lock()
	infos := make([]CronInfo, 0, len(s.entries))
	matchers := make([]cronMatcher, 0, len(s.entries))
	for _, e := range s.entries {
		if !tracing.VisibleTo(e.Session, session) {
			continue
		}
		infos = append(infos, CronInfo{
			ID: e.ID, Session: e.Session, Name: e.Name, Spec: e.Spec, Prompt: e.Prompt,
			CreatedAt: e.createdAt, LastFiredAt: e.lastFired,
		})
		matchers = append(matchers, e.matcher)
	}
	s.mu.Unlock()
	for i := range infos {
		if next, ok := matchers[i].next(now, cronNextHorizon); ok {
			infos[i].NextFireAt = next
		}
	}
	return infos
}

// cronNextHorizon bounds the next-fire minute scan. Ten years covers every
// real-world gap between matching dates (the rarest sane spec, February
// 29th, recurs at most eight years apart) while ordinary schedules match
// within days and never approach the bound.
const cronNextHorizon = 10 * 365 * 24 * time.Hour

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
	onChange := s.onChange
	s.mu.Unlock()
	for _, e := range due {
		if onChange != nil {
			onChange(e.Session)
		}
		s.notify(e.Session, "scheduled-task",
			fmt.Sprintf("Scheduled task %s (%s) is due now:\n%s", e.displayName(), e.Spec, e.Prompt))
	}
}

// Agent delegates: the tool wiring, Close() teardown and the tests keep
// their entry points; the mechanics live on the scheduler. The entry is
// owned by the session of the run that scheduled it, and list/delete are
// scoped the same way: the session on the ctx sees its own entries plus
// the unscoped ones ("" sees everything).
func (a *Agent) addCron(ctx context.Context, name, prompt, spec string) (string, error) {
	return a.cron.Add(sessionFromCtx(ctx), name, prompt, spec)
}

func (a *Agent) deleteCron(ctx context.Context, id string) bool {
	return a.cron.Delete(id, sessionFromCtx(ctx))
}

func (a *Agent) listCron(ctx context.Context) []cronEntry {
	return a.cron.List(sessionFromCtx(ctx))
}

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

// next returns the first matching minute strictly after `after`, scanning
// minute by minute in local time up to horizon; ok=false when no matching
// minute exists within the horizon (e.g. "0 0 31 2 *": February 31st never
// exists). The scan starts at the next whole minute so an entry firing at
// `after`'s minute reports its following occurrence.
func (m cronMatcher) next(after time.Time, horizon time.Duration) (time.Time, bool) {
	t := after.Truncate(time.Minute).Add(time.Minute)
	limit := after.Add(horizon)
	for !t.After(limit) {
		if m.matches(t) {
			return t, true
		}
		t = t.Add(time.Minute)
	}
	return time.Time{}, false
}

const scheduleCronDescription = `Schedule a recurring or one-shot scheduled prompt using a cron expression (5 fields: minute hour day-of-month month day-of-week, local time). When an entry is due, its prompt is delivered to you as a <scheduled-task> notification — act on it like new user input. Entries live for the current session only and are scoped to the creating session: list and delete only see entries your session created (plus unscoped ones from single-session surfaces).

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
				if a.deleteCron(ctx, args.DeleteID) {
					return coretools.Result{Content: "Deleted " + args.DeleteID}, nil
				}
				return coretools.Result{Content: "Error: no scheduled entry " + args.DeleteID}, nil
			}
			if args.List {
				entries := a.listCron(ctx)
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
			id, err := a.addCron(ctx, args.Name, args.Prompt, args.Cron)
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
