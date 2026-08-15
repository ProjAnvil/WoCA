package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/session"
)

// sessionStore persists agent transcripts to JSONL files (one per session),
// with an in-memory cache. Workspace metadata (which project a session
// belongs to) lives in a sidecar index.json so the transcript format stays a
// plain message-per-line stream. When dir is empty, persistence is disabled
// and sessions live only in memory.
type sessionStore struct {
	dir string
	// defaultWorkspace labels sessions created before the workspace index
	// existed (legacy transcripts without metadata).
	defaultWorkspace string

	mu  sync.Mutex
	mem map[string][]messages.Message
}

func newSessionStore(dir string) *sessionStore {
	return &sessionStore{dir: dir, mem: map[string][]messages.Message{}}
}

func (s *sessionStore) path(id string) string {
	return filepath.Join(s.dir, id+".jsonl")
}

func (s *sessionStore) indexPath() string {
	return filepath.Join(s.dir, "index.json")
}

// load returns a session transcript from memory or disk (nil if absent).
func (s *sessionStore) load(id string) []messages.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	if msgs, ok := s.mem[id]; ok {
		return msgs
	}
	if s.dir == "" {
		return nil
	}
	msgs, err := session.Load(s.path(id))
	if err != nil {
		return nil
	}
	s.mem[id] = msgs
	return msgs
}

// save persists a session transcript to memory and (if enabled) disk. When
// workspace is non-empty it is recorded in the metadata index for grouping.
func (s *sessionStore) save(id string, msgs []messages.Message, workspace string) error {
	s.mu.Lock()
	s.mem[id] = msgs
	s.mu.Unlock()
	if s.dir == "" {
		return nil
	}
	if err := session.Save(s.path(id), msgs); err != nil {
		return err
	}
	if workspace != "" {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.recordMeta(sessionMeta{
			ID:            id,
			Workspace:     workspace,
			WorkspaceName: workspaceDisplayName(workspace),
			UpdatedAt:     time.Now().Format(time.RFC3339),
		})
	}
	return nil
}

// sessionMeta is the persisted workspace metadata for one session.
type sessionMeta struct {
	ID            string `json:"id"`
	Workspace     string `json:"workspace"`      // absolute workdir path
	WorkspaceName string `json:"workspace_name"` // display name (dir basename)
	UpdatedAt     string `json:"updated_at"`
}

// loadIndex reads the sidecar metadata index (empty map when absent).
func (s *sessionStore) loadIndex() map[string]sessionMeta {
	out := map[string]sessionMeta{}
	if s.dir == "" {
		return out
	}
	b, err := os.ReadFile(s.indexPath())
	if err != nil {
		return out
	}
	var list []sessionMeta
	if err := json.Unmarshal(b, &list); err != nil {
		return out
	}
	for _, m := range list {
		out[m.ID] = m
	}
	return out
}

// recordMeta upserts one entry and rewrites index.json atomically. Callers
// must hold s.mu.
func (s *sessionStore) recordMeta(meta sessionMeta) {
	index := s.loadIndex()
	if old, ok := index[meta.ID]; ok {
		if meta.Workspace == "" {
			meta.Workspace = old.Workspace
			meta.WorkspaceName = old.WorkspaceName
		}
	}
	index[meta.ID] = meta

	list := make([]sessionMeta, 0, len(index))
	for _, m := range index {
		list = append(list, m)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return
	}
	tmp := s.indexPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, s.indexPath())
}

// workspaceDisplayName derives a human-friendly workspace label from a path.
func workspaceDisplayName(workspace string) string {
	ws := strings.TrimSpace(workspace)
	if ws == "" {
		return "未分类工作区"
	}
	name := filepath.Base(ws)
	if name == "." || name == string(filepath.Separator) || name == "" {
		return ws
	}
	return name
}

// sessionSummary is a lightweight description for listing.
type sessionSummary struct {
	ID            string `json:"id"`
	Title         string `json:"title"`
	UpdatedAt     string `json:"updated_at"`
	MessageCount  int    `json:"message_count"`
	Workspace     string `json:"workspace"`
	WorkspaceName string `json:"workspace_name"`
}

// goalPath is the per-session goal sidecar (goal-mode persistence): the
// objective, state-machine status, budgets, and per-cycle progress live next
// to the transcript so a server restart can restore the goal.
func (s *sessionStore) goalPath(id string) string {
	return filepath.Join(s.dir, id+".goal.json")
}

// saveGoal persists the goal sidecar atomically (nil deletes it).
func (s *sessionStore) saveGoal(id string, g *agent.Goal) {
	if s.dir == "" {
		return
	}
	if g == nil {
		_ = os.Remove(s.goalPath(id))
		return
	}
	b, err := json.MarshalIndent(g, "", "  ")
	if err != nil {
		return
	}
	tmp := s.goalPath(id) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, s.goalPath(id))
}

// loadGoal reads the persisted goal sidecar (nil when absent).
func (s *sessionStore) loadGoal(id string) *agent.Goal {
	if s.dir == "" {
		return nil
	}
	b, err := os.ReadFile(s.goalPath(id))
	if err != nil {
		return nil
	}
	var g agent.Goal
	if err := json.Unmarshal(b, &g); err != nil {
		return nil
	}
	return &g
}

// list returns persisted sessions, newest first, each tagged with the
// workspace it belongs to (the server's workdir for legacy sessions).
func (s *sessionStore) list() ([]sessionSummary, error) {
	if s.dir == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	index := s.loadIndex()
	out := make([]sessionSummary, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".jsonl")
		msgs, err := session.Load(filepath.Join(s.dir, e.Name()))
		if err != nil {
			continue
		}
		updatedAt := ""
		if info, err := e.Info(); err == nil {
			updatedAt = info.ModTime().Format(time.RFC3339)
		}
		workspace, workspaceName := s.workspaceOf(index, id)
		out = append(out, sessionSummary{
			ID:            id,
			Title:         sessionTitle(msgs),
			UpdatedAt:     updatedAt,
			MessageCount:  len(msgs),
			Workspace:     workspace,
			WorkspaceName: workspaceName,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt > out[j].UpdatedAt })
	return out, nil
}

// workspaceOf resolves a session's workspace from the index, falling back to
// the store's default (the current server workdir) for legacy sessions.
func (s *sessionStore) workspaceOf(index map[string]sessionMeta, id string) (string, string) {
	if m, ok := index[id]; ok && m.Workspace != "" {
		return m.Workspace, m.WorkspaceName
	}
	if s.defaultWorkspace != "" {
		return s.defaultWorkspace, workspaceDisplayName(s.defaultWorkspace)
	}
	return "", "未分类工作区"
}

func sessionTitle(msgs []messages.Message) string {
	for _, m := range msgs {
		if m.Role == messages.RoleHuman && strings.TrimSpace(m.Content) != "" {
			return truncateRunes(m.Content, 40)
		}
	}
	return "新会话"
}

func truncateRunes(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
