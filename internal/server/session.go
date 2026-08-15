package server

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/session"
)

// sessionStore persists agent transcripts to JSONL files (one per session),
// with an in-memory cache. When dir is empty, persistence is disabled and
// sessions live only in memory.
type sessionStore struct {
	dir string

	mu  sync.Mutex
	mem map[string][]messages.Message
}

func newSessionStore(dir string) *sessionStore {
	return &sessionStore{dir: dir, mem: map[string][]messages.Message{}}
}

func (s *sessionStore) path(id string) string {
	return filepath.Join(s.dir, id+".jsonl")
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

// save persists a session transcript to memory and (if enabled) disk.
func (s *sessionStore) save(id string, msgs []messages.Message) error {
	s.mu.Lock()
	s.mem[id] = msgs
	s.mu.Unlock()
	if s.dir == "" {
		return nil
	}
	return session.Save(s.path(id), msgs)
}

// sessionSummary is a lightweight description for listing.
type sessionSummary struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	UpdatedAt    string `json:"updated_at"`
	MessageCount int    `json:"message_count"`
}

// list returns persisted sessions, newest first.
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
		out = append(out, sessionSummary{
			ID:           id,
			Title:        sessionTitle(msgs),
			UpdatedAt:    updatedAt,
			MessageCount: len(msgs),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt > out[j].UpdatedAt })
	return out, nil
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
