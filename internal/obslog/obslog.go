// Package obslog implements woca's harness observability log: a process-wide
// store (disabled by default) that records graph-node dispatches, state
// transitions, tool activity, and full message dumps under ~/.woca/logs.
//
// Layout — one file family per detail level, all size-rotated:
//
//	logs/woca.log                          main log: process + run + summary events
//	logs/ws-<name>-<hash8>/<session>.jsonl session trace: one JSON event per line
//	logs/ws-<name>-<hash8>/<session>/      message-level dumps: <run>.json files
//
// The main log and every session trace rotate by size (path → path.1 → …,
// keeping the newest N). Message dumps are per-run snapshots, overwritten on
// re-runs of the same key, so they need no rotation.
package obslog

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/graph"
)

// Rotation defaults.
const (
	DefaultMaxBytes = int64(8 << 20) // 8 MB per file
	DefaultKeep     = 5              // newest 5 files per family
)

// Store is the process-wide observability recorder. All methods are safe for
// concurrent use and cheap no-ops while disabled.
type Store struct {
	root     string
	enabled  atomic.Bool
	locked   bool // --debug-log forces it on; dynamic refresh cannot disable
	maxBytes int64
	keep     int

	mu      sync.Mutex
	writers map[string]*rotatingWriter

	ctxMu sync.Mutex
	cur   Ctx
}

// Ctx tags events with the run's scope: which workspace, session, and run
// produced them. Harnesses set it at run start (agent.SetObsRun).
type Ctx struct {
	Workspace     string // absolute workdir path
	WorkspaceName string // display name (dir basename)
	Session       string // session id (or "tui")
	Run           string // run id
}

// New creates a Store rooted at root (~/.woca/logs). Files are created lazily
// on the first write while enabled.
func New(root string, enabled bool) *Store {
	s := &Store{
		root:     root,
		maxBytes: DefaultMaxBytes,
		keep:     DefaultKeep,
		writers:  map[string]*rotatingWriter{},
	}
	s.enabled.Store(enabled)
	return s
}

// Root returns the log root directory.
func (s *Store) Root() string { return s.root }

// Enabled reports whether full logging is on.
func (s *Store) Enabled() bool { return s != nil && s.enabled.Load() }

// SetEnabled toggles full logging at runtime (settings API, /debug-log).
// Enabling writes a marker event so the log self-documents its own start.
// A store locked open by the --debug-log flag ignores disable attempts.
func (s *Store) SetEnabled(v bool) {
	if s == nil {
		return
	}
	if !v && s.locked {
		return
	}
	s.enabled.Store(v)
	if v {
		s.Log("info", "observability_enabled", map[string]any{"root": s.root, "rotate_max_bytes": s.maxBytes, "rotate_keep": s.keep})
	}
}

// LockOpen pins the store on (--debug-log flag): later SetEnabled(false)
// calls — e.g. dynamic refreshes from the settings database — are ignored,
// keeping the flag's precedence for the whole process lifetime.
func (s *Store) LockOpen() {
	if s == nil {
		return
	}
	s.locked = true
	s.SetEnabled(true)
}

// SetContext records the scope subsequent Log/Dump calls are attributed to.
func (s *Store) SetContext(ws, wsName, session, run string) {
	if s == nil {
		return
	}
	s.ctxMu.Lock()
	s.cur = Ctx{Workspace: ws, WorkspaceName: wsName, Session: session, Run: run}
	s.ctxMu.Unlock()
}

func (s *Store) context() Ctx {
	s.ctxMu.Lock()
	defer s.ctxMu.Unlock()
	return s.cur
}

// Event is one JSONL line of the session trace / main log.
type Event struct {
	TS        time.Time      `json:"ts"`
	Level     string         `json:"level"`
	Workspace string         `json:"workspace,omitempty"`
	Session   string         `json:"session,omitempty"`
	Run       string         `json:"run,omitempty"`
	Event     string         `json:"event"`
	Data      map[string]any `json:"data,omitempty"`
}

// Log records one event into the main log and, when a session context is set,
// into that session's trace file. No-op while disabled.
func (s *Store) Log(level, event string, data map[string]any) {
	if !s.Enabled() {
		return
	}
	ctx := s.context()
	ev := Event{
		TS:        time.Now().UTC(),
		Level:     level,
		Workspace: ctx.Workspace,
		Session:   ctx.Session,
		Run:       ctx.Run,
		Event:     event,
		Data:      data,
	}
	line, err := json.Marshal(ev)
	if err != nil {
		return
	}
	line = append(line, '\n')
	s.write("woca.log", line)
	if ctx.Session != "" && ctx.Workspace != "" {
		s.write(filepath.Join(workspaceDir(ctx.Workspace), sanitize(ctx.Session)+".jsonl"), line)
	}
}

// DumpMessages persists the message-level snapshot for one run: the full
// transcript (roles, content, tool calls, usage) plus the graph-state
// summary, written atomically as <workspace>/<session>/<run>.json.
func (s *Store) DumpMessages(msgs []messages.Message, stateSummary map[string]any) {
	if !s.Enabled() {
		return
	}
	ctx := s.context()
	if ctx.Session == "" || ctx.Workspace == "" || ctx.Run == "" {
		return
	}
	payload := map[string]any{
		"ts":         time.Now().UTC().Format(time.RFC3339Nano),
		"workspace":  ctx.Workspace,
		"session":    ctx.Session,
		"run":        ctx.Run,
		"state":      stateSummary,
		"messages":   msgs,
		"message_count": len(msgs),
	}
	b, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return
	}
	dir := filepath.Join(s.root, workspaceDir(ctx.Workspace), sanitize(ctx.Session))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	tmp := filepath.Join(dir, sanitize(ctx.Run)+".json.tmp")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, filepath.Join(dir, sanitize(ctx.Run)+".json"))
}

// write appends bytes to root-relative path through a cached rotating writer.
func (s *Store) write(rel string, b []byte) {
	s.mu.Lock()
	w, ok := s.writers[rel]
	if !ok {
		w = &rotatingWriter{path: filepath.Join(s.root, rel), maxBytes: s.maxBytes, keep: s.keep}
		s.writers[rel] = w
	}
	s.mu.Unlock()
	_, _ = w.Write(b)
}

// --- graph.Observer bridge ---

// ObserveNode implements graph.Observer: every node dispatch of every
// observed graph (the agent graph, the goal graph) becomes a trace event.
func (s *Store) ObserveNode(ev graph.NodeEvent) {
	if !s.Enabled() {
		return
	}
	data := map[string]any{
		"graph":  ev.Graph,
		"node":   ev.Node,
		"phase":  ev.Phase,
	}
	if ev.DurationMS > 0 {
		data["duration_ms"] = ev.DurationMS
	}
	if ev.Err != "" {
		data["error"] = ev.Err
	}
	if ev.Phase == graph.PhaseStart {
		data["messages"] = len(messagesOf(ev.Values["messages"]))
	} else if upd := updateOf(ev.Update); upd != nil {
		data["update_keys"] = keysOf(upd)
		if msgs := messagesOf(upd["messages"]); len(msgs) > 0 {
			data["update"] = summarizeMessages(msgs, 40)
		}
	}
	s.Log("debug", "graph_node", data)
}

// LogUsage records one model call's token accounting.
func (s *Store) LogUsage(model string, um messages.UsageMetadata) {
	if !s.Enabled() || (um.TotalTokens == 0 && um.InputTokens == 0 && um.OutputTokens == 0) {
		return
	}
	data := map[string]any{
		"model":  model,
		"input":  um.InputTokens,
		"output": um.OutputTokens,
		"total":  um.TotalTokens,
	}
	if um.InputTokenDetails != nil {
		data["cache_read"] = um.InputTokenDetails.CacheReadInputTokens
		data["cache_write"] = um.InputTokenDetails.CacheCreationInputTokens
	}
	s.Log("debug", "model_usage", data)
}

func messagesOf(v any) []messages.Message {
	msgs, _ := v.([]messages.Message)
	return msgs
}

func updateOf(v any) graph.State {
	switch t := v.(type) {
	case graph.State:
		return t
	case graph.Command:
		return t.Update
	case *graph.Command:
		return t.Update
	}
	return nil
}

func keysOf(state graph.State) []string {
	out := make([]string, 0, len(state))
	for k := range state {
		out = append(out, k)
	}
	return out
}

// summarizeMessages renders bounded per-message summaries for trace events.
func summarizeMessages(msgs []messages.Message, max int) []map[string]any {
	out := make([]map[string]any, 0, len(msgs))
	for i, m := range msgs {
		if i >= max {
			out = append(out, map[string]any{"role": m.Role, "truncated": true})
			break
		}
		item := map[string]any{
			"seq":     i,
			"role":    string(m.Role),
			"content": preview(m.Content, 240),
		}
		if m.ID != "" {
			item["id"] = m.ID
		}
		if len(m.ToolCalls) > 0 {
			calls := make([]map[string]any, 0, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				calls = append(calls, map[string]any{"id": tc.ID, "name": tc.Name, "args": preview(fmt.Sprintf("%v", tc.Args), 200)})
			}
			item["tool_calls"] = calls
		}
		if m.ToolCallID != "" {
			item["tool_call_id"] = m.ToolCallID
		}
		if um := m.UsageMetadata; um.TotalTokens > 0 {
			item["usage"] = map[string]any{"in": um.InputTokens, "out": um.OutputTokens, "total": um.TotalTokens}
		}
		out = append(out, item)
	}
	return out
}

func preview(s string, max int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// --- workspace/session path helpers ---

var unsafeName = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

// workspaceDir maps a workspace path to its log directory name:
// ws-<basename>-<hash8> (hash keeps same-named dirs distinct).
func workspaceDir(ws string) string {
	sum := sha1.Sum([]byte(ws))
	base := unsafeName.ReplaceAllString(filepath.Base(strings.TrimRight(ws, "/")), "-")
	if base == "" || base == "." || base == "-" {
		base = "default"
	}
	if len(base) > 32 {
		base = base[:32]
	}
	return fmt.Sprintf("ws-%s-%s", base, hex.EncodeToString(sum[:])[:8])
}

func sanitize(name string) string {
	name = unsafeName.ReplaceAllString(name, "-")
	return strings.Trim(name, "-")
}

// --- rotating writer ---

// rotatingWriter is a size-rotated file writer: on overflow the file chain
// shifts (path → path.1 → path.2 → …) and the oldest is dropped.
type rotatingWriter struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	keep     int
	f        *os.File
	size     int64
}

func (w *rotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		if err := w.openLocked(); err != nil {
			return 0, err
		}
	}
	if w.maxBytes > 0 && w.keep > 0 && w.size+int64(len(p)) > w.maxBytes {
		w.rotateLocked()
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	return n, err
}

func (w *rotatingWriter) openLocked() error {
	if err := os.MkdirAll(filepath.Dir(w.path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	w.f = f
	w.size = info.Size()
	return nil
}

// rotateLocked shifts the backup chain and reopens a fresh file. Best-effort:
// on rename failure the writer keeps appending to the current file.
func (w *rotatingWriter) rotateLocked() {
	if w.f != nil {
		_ = w.f.Close()
		w.f = nil
	}
	for i := w.keep; i >= 2; i-- {
		_ = os.Rename(fmt.Sprintf("%s.%d", w.path, i-1), fmt.Sprintf("%s.%d", w.path, i))
	}
	_ = os.Rename(w.path, w.path+".1")
	_ = os.Remove(fmt.Sprintf("%s.%d", w.path, w.keep+1))
	if err := w.openLocked(); err != nil {
		w.f = nil
	}
	w.size = 0
}
