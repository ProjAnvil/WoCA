// Package session persists agent transcripts to JSON-lines files.
package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/projanvil/langchain-golang/core/messages"
	"github.com/projanvil/woca/internal/platform/blobstore"
)

// DefaultDir returns the harness data directory (~/.woca).
func DefaultDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".woca")
}

// Encode renders a transcript as JSON lines (one message per line) — the
// single codec shared by the file helpers and the blobstore path, so
// on-disk bytes are identical either way.
func Encode(msgs []messages.Message) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, m := range msgs {
		if err := enc.Encode(m); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}

// Decode parses a JSON-lines transcript produced by Encode.
func Decode(data []byte) ([]messages.Message, error) {
	var out []messages.Message
	dec := json.NewDecoder(bytes.NewReader(data))
	for {
		var m messages.Message
		if err := dec.Decode(&m); err != nil {
			if errors.Is(err, io.EOF) {
				return out, nil
			}
			return nil, err
		}
		out = append(out, m)
	}
}

// Save writes a transcript (one message per JSON line) to path, creating
// parent directories as needed.
func Save(path string, msgs []messages.Message) error {
	data, err := Encode(msgs)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// SaveBlob stores a transcript under key in a blob store.
func SaveBlob(ctx context.Context, store blobstore.Store, key string, msgs []messages.Message) error {
	data, err := Encode(msgs)
	if err != nil {
		return err
	}
	return store.Put(ctx, key, data)
}

// LoadBlob reads a transcript written by SaveBlob (blobstore.ErrNotFound
// when the key has no transcript).
func LoadBlob(ctx context.Context, store blobstore.Store, key string) ([]messages.Message, error) {
	data, err := store.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	return Decode(data)
}

// Load reads a transcript written by Save.
func Load(path string) ([]messages.Message, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Decode(data)
}

// Todo and Task are plain persisted representations (no dependency on other
// woca packages).
type Todo struct {
	Content    string `json:"content"`
	Status     string `json:"status"`
	ActiveForm string `json:"active_form,omitempty"`
}

type Task struct {
	ID          string `json:"id"`
	Subject     string `json:"subject"`
	Description string `json:"description"`
	Status      string `json:"status"`
}

// State is a full resumable session snapshot.
type State struct {
	Messages []messages.Message `json:"messages"`
	PlanMode bool               `json:"plan_mode,omitempty"`
	Todos    []Todo             `json:"todos,omitempty"`
	Tasks    []Task             `json:"tasks,omitempty"`
}

// SaveState writes a single JSON object (not JSON lines) to path, creating
// parent directories as needed.
func SaveState(path string, s State) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	return json.NewEncoder(f).Encode(s)
}

// LoadState reads a snapshot written by SaveState. A missing file returns
// (State{}, nil).
func LoadState(path string) (State, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return State{}, nil
		}
		return State{}, err
	}
	defer func() { _ = f.Close() }()

	var s State
	if err := json.NewDecoder(f).Decode(&s); err != nil {
		return State{}, err
	}
	return s, nil
}
