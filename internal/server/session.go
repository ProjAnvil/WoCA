package server

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/projanvil/langchain-golang/core/messages"
	"github.com/rs/zerolog"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/platform/blobstore"
	"github.com/projanvil/woca/internal/session"
)

// sessionStore persists agent transcripts to JSONL files (one per session),
// with an in-memory cache. Workspace metadata (which project a session
// belongs to) lives in a sidecar index.json so the transcript format stays a
// plain message-per-line stream; the in-memory index is the single source of
// truth for listings (legacy transcripts are folded in at startup). When dir
// is empty, persistence is disabled and sessions live only in memory.
type sessionStore struct {
	dir string
	// defaultWorkspace labels sessions created before the workspace index
	// existed (legacy transcripts without metadata).
	defaultWorkspace string
	// testAfterSave, when set, fires at the end of every successful save
	// (after recordMeta) — a test hook for release-timing assertions; it
	// must not call back into the store.
	testAfterSave func(id string)
	// blobs stores transcript bodies (Phase 2 T5). Nil when persistence is
	// disabled (dir == "") or the root could not be created (blobsErr then
	// carries the construction error).
	blobs    blobstore.Store
	blobsErr error
	// log receives debug-level persistence-failure reports (delete is
	// best-effort by contract); defaults to a no-op logger until the
	// owning Server wires its own in.
	log zerolog.Logger

	// mu guards mem, tombstones and index ONLY — no file I/O under it.
	mu         sync.Mutex
	mem        map[string][]messages.Message
	tombstones map[string]bool
	index      map[string]sessionMeta // single source of truth; loadIndex + reconciliation at startup

	// writeMu serializes index.json write+rename so concurrent recorders
	// never interleave the tmp file.
	writeMu sync.Mutex
}

func newSessionStore(dir string) *sessionStore {
	s := &sessionStore{
		dir:        dir,
		mem:        map[string][]messages.Message{},
		tombstones: map[string]bool{},
		index:      map[string]sessionMeta{},
		log:        zerolog.Nop(),
	}
	if dir != "" {
		// Transcripts live behind the blobstore seam (Phase 2 T5): the
		// local directory store keeps the on-disk layout byte-identical,
		// so existing JSONL files need no migration; an S3-backed store is
		// a drop-in replacement for multi-instance deployments.
		blobs, err := blobstore.NewLocal(dir)
		if err != nil {
			// The transcript root cannot be created: keep dir set so every
			// persistence attempt reports the failure (a broken root is an
			// error, not a silent fallback to memory-only).
			s.blobsErr = err
		} else {
			s.blobs = blobs
		}
		s.index = s.loadIndex()
		s.reconcileOnStartup()
	}
	return s
}

// reconcileOnStartup folds transcripts that have no index entry (legacy
// files, or files written before the in-memory index existed) into the
// index once, so list() never has to touch .jsonl files at request time.
// It also prunes zombie entries — index records whose transcript file no
// longer exists (a crash between the transcript delete and the index
// flush leaves them behind; listing them would show sessions that load as
// nil).
func (s *sessionStore) reconcileOnStartup() {
	if s.blobs == nil {
		return
	}
	// Startup reconcile is not tied to any request: background context.
	keys, err := s.blobs.List(context.Background(), "")
	if err != nil {
		return
	}
	changed := false
	for _, key := range keys {
		if !strings.HasSuffix(key, ".jsonl") || strings.ContainsAny(key, `/\`) {
			continue
		}
		id := strings.TrimSuffix(key, ".jsonl")
		if _, ok := s.index[id]; ok {
			continue
		}
		msgs, err := session.LoadBlob(context.Background(), s.blobs, key)
		if err != nil {
			continue
		}
		updatedAt := ""
		s.index[id] = sessionMeta{
			ID:           id,
			UpdatedAt:    updatedAt,
			Title:        sessionTitle(msgs),
			MessageCount: len(msgs),
		}
		changed = true
	}
	// Prune zombie entries: sessions in the index with no corresponding
	// .jsonl blob (filtered before the index is written back).
	if s.blobs != nil {
		for id := range s.index {
			if _, err := s.blobs.Get(context.Background(), s.key(id)); err != nil {
				delete(s.index, id)
				changed = true
			}
		}
	}
	if changed {
		s.flushIndex()
	}
}

// key is the blobstore key of a session transcript (identical filename to
// the legacy layout).
func (s *sessionStore) key(id string) string {
	return id + ".jsonl"
}

func (s *sessionStore) indexPath() string {
	return filepath.Join(s.dir, "index.json")
}

// load returns a session transcript from memory or disk (nil if absent).
func (s *sessionStore) load(ctx context.Context, id string) []messages.Message {
	s.mu.Lock()
	if msgs, ok := s.mem[id]; ok {
		s.mu.Unlock()
		return msgs
	}
	s.mu.Unlock()
	if s.dir == "" {
		return nil
	}
	msgs, err := session.LoadBlob(ctx, s.blobs, s.key(id)) // blob read outside the lock
	if err != nil {
		return nil
	}
	s.mu.Lock()
	s.mem[id] = msgs
	s.mu.Unlock()
	return msgs
}

// save persists a session transcript to memory and (if enabled) disk. When
// workspace is non-empty it is recorded in the metadata index for grouping.
// Saves for deleted sessions are dropped (see tombstones).
func (s *sessionStore) save(ctx context.Context, id string, msgs []messages.Message, workspace string) error {
	s.mu.Lock()
	if s.tombstones[id] {
		s.mu.Unlock()
		return nil
	}
	s.mem[id] = msgs
	s.mu.Unlock()
	if s.dir == "" {
		return nil
	}
	if s.blobs == nil {
		return s.blobsErr
	}
	if err := session.SaveBlob(ctx, s.blobs, s.key(id), msgs); err != nil {
		return err
	}
	if workspace != "" {
		s.recordMeta(sessionMeta{
			ID:            id,
			Workspace:     workspace,
			WorkspaceName: workspaceDisplayName(workspace),
			UpdatedAt:     time.Now().Format(time.RFC3339),
			Title:         sessionTitle(msgs),
			MessageCount:  len(msgs),
		})
	}
	if s.testAfterSave != nil {
		s.testAfterSave(id)
	}
	return nil
}

// sessionMeta is the persisted workspace metadata for one session.
type sessionMeta struct {
	ID            string `json:"id"`
	Workspace     string `json:"workspace"`      // absolute workdir path
	WorkspaceName string `json:"workspace_name"` // display name (dir basename)
	UpdatedAt     string `json:"updated_at"`
	Title         string `json:"title,omitempty"`
	MessageCount  int    `json:"message_count,omitempty"` // absent in old indexes shows 0; self-heals on next save
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

// recordMeta upserts one index entry in memory, then persists the index via
// the serial writer. The map mutation happens under mu (no I/O); marshal and
// write+rename happen under writeMu only.
func (s *sessionStore) recordMeta(meta sessionMeta) {
	s.mu.Lock()
	if old, ok := s.index[meta.ID]; ok {
		if meta.Workspace == "" {
			meta.Workspace = old.Workspace
			meta.WorkspaceName = old.WorkspaceName
		}
		if meta.Title == "" {
			meta.Title = old.Title
		}
	}
	s.index[meta.ID] = meta
	s.mu.Unlock()
	s.flushIndex()
}

// flushIndex snapshots the index and rewrites index.json atomically. The
// writeMu serialization prevents tmp-file interleave between recorders.
func (s *sessionStore) flushIndex() {
	if s.dir == "" {
		return
	}
	s.mu.Lock()
	snapshot := make(map[string]sessionMeta, len(s.index))
	maps.Copy(snapshot, s.index)
	s.mu.Unlock()
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.writeIndex(snapshot)
}

// delete removes a session: the in-memory transcript, the .jsonl file, the
// goal sidecar and the index entry. The tombstone keeps a run still in flight
// from resurrecting the transcript via its completion save.
func (s *sessionStore) delete(ctx context.Context, id string) {
	s.mu.Lock()
	delete(s.mem, id)
	delete(s.index, id)
	s.tombstones[id] = true
	s.mu.Unlock()
	if s.dir == "" {
		return
	}
	// Best-effort cleanup by contract (deleting an unknown id still
	// succeeds), but failures must not vanish silently. Not-found is the
	// success path for unknown ids, not a failure.
	if err := s.blobs.Delete(ctx, s.key(id)); err != nil && !errors.Is(err, blobstore.ErrNotFound) {
		s.log.Debug().Err(err).Str("session", id).Msg("transcript blob delete failed")
	}
	if err := os.Remove(s.goalPath(id)); err != nil && !os.IsNotExist(err) {
		s.log.Debug().Err(err).Str("session", id).Msg("goal sidecar remove failed")
	}
	s.flushIndex()
}

// writeIndex rewrites index.json atomically. Callers must hold s.writeMu;
// the argument is a snapshot.
func (s *sessionStore) writeIndex(index map[string]sessionMeta) {
	list := make([]sessionMeta, 0, len(index))
	for _, m := range index {
		list = append(list, m)
	}
	slices.SortFunc(list, func(a, b sessionMeta) int { return cmp.Compare(a.ID, b.ID) })
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
	// RunStatus is the durable run state ("running" | "awaiting"; empty =
	// idle) and RunID the run behind it — set from the session_state store.
	RunStatus string `json:"run_status,omitempty"`
	RunID     string `json:"run_id,omitempty"`
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

// list returns sessions from the in-memory index, newest first — zero JSONL
// reads. Legacy sessions enter the index via startup reconcile.
func (s *sessionStore) list() ([]sessionSummary, error) {
	s.mu.Lock()
	metas := make([]sessionMeta, 0, len(s.index))
	for _, m := range s.index {
		metas = append(metas, m)
	}
	s.mu.Unlock()
	out := make([]sessionSummary, 0, len(metas))
	for _, m := range metas {
		workspace, workspaceName := s.workspaceOfMeta(m)
		title := m.Title
		if title == "" {
			title = "新会话"
		}
		out = append(out, sessionSummary{
			ID:            m.ID,
			Title:         title,
			UpdatedAt:     m.UpdatedAt,
			MessageCount:  m.MessageCount,
			Workspace:     workspace,
			WorkspaceName: workspaceName,
		})
	}
	slices.SortFunc(out, func(a, b sessionSummary) int { return cmp.Compare(b.UpdatedAt, a.UpdatedAt) })
	return out, nil
}

// workspaceOfMeta resolves a session's workspace from its index entry,
// falling back to the store default for legacy sessions.
func (s *sessionStore) workspaceOfMeta(m sessionMeta) (string, string) {
	if m.Workspace != "" {
		return m.Workspace, m.WorkspaceName
	}
	if s.defaultWorkspace != "" {
		return s.defaultWorkspace, workspaceDisplayName(s.defaultWorkspace)
	}
	return "", "未分类工作区"
}

// workspaceOf resolves one session's absolute workspace path ("" when
// unknown). The context-breakdown endpoint uses it to rebuild the per-run
// workspace context the agent's prompt assembly keys off.
func (s *sessionStore) workspaceOf(id string) string {
	s.mu.Lock()
	meta, ok := s.index[id]
	s.mu.Unlock()
	if ok && meta.Workspace != "" {
		return meta.Workspace
	}
	return s.defaultWorkspace
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
