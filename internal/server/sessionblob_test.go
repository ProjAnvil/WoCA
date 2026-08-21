package server

import (
	"context"
	"path/filepath"
	"sort"
	"sync"
	"testing"

	"github.com/projanvil/langchain-golang/core/messages"
	"github.com/projanvil/woca/internal/platform/blobstore"
	"github.com/projanvil/woca/internal/session"
)

// memBlobs is an in-memory blobstore.Store recording every key touched.
type memBlobs struct {
	mu    sync.Mutex
	data  map[string][]byte
	order []string
}

func newMemBlobs() *memBlobs { return &memBlobs{data: map[string][]byte{}} }

func (m *memBlobs) Put(_ context.Context, key string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, seen := m.data[key]; !seen {
		m.order = append(m.order, key)
	}
	m.data[key] = data
	return nil
}

func (m *memBlobs) Get(_ context.Context, key string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.data[key]
	if !ok {
		return nil, blobstore.ErrNotFound
	}
	return b, nil
}

func (m *memBlobs) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, key)
	return nil
}

func (m *memBlobs) List(_ context.Context, prefix string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for k := range m.data {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out, nil
}

// Transcripts must flow through the blobstore seam under the legacy key
// layout (<id>.jsonl), and deletes must remove the blob.
func TestSessionStoreUsesBlobstoreSeam(t *testing.T) {
	s := newSessionStore("")
	blobs := newMemBlobs()
	s.blobs = blobs
	s.dir = t.TempDir()

	msgs := []messages.Message{messages.Human("hello"), messages.AI("world")}
	if err := s.save("sess-1", msgs, ""); err != nil {
		t.Fatal(err)
	}
	if b, ok := blobs.data["sess-1.jsonl"]; !ok || len(b) == 0 {
		t.Fatalf("transcript blob missing: %v", blobs.data)
	}
	got := s.load("sess-1")
	if len(got) != 2 || got[0].Content != "hello" {
		t.Fatalf("load = %+v", got)
	}
	s.delete("sess-1")
	if _, ok := blobs.data["sess-1.jsonl"]; ok {
		t.Fatal("transcript blob survived remove")
	}
}

// Migration: transcripts written by the legacy direct-file path (before the
// blobstore seam) must load unchanged — the local store points at the same
// directory with the same key layout, so no data migration is needed.
func TestLegacyTranscriptsLoadThroughBlobstore(t *testing.T) {
	dir := t.TempDir()
	if err := session.Save(filepath.Join(dir, "legacy.jsonl"),
		[]messages.Message{messages.Human("old world")}); err != nil {
		t.Fatal(err)
	}
	s := newSessionStore(dir)
	got := s.load("legacy")
	if len(got) != 1 || got[0].Content != "old world" {
		t.Fatalf("legacy transcript = %+v", got)
	}
	if _, ok := s.index["legacy"]; !ok {
		t.Fatal("legacy transcript not reconciled into the index")
	}
}
