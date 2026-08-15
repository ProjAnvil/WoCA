// Package session persists agent transcripts to JSON-lines files.
package session

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"

	"github.com/projanvil/langchain-golang/core/messages"
)

// DefaultDir returns the harness data directory (~/.woca).
func DefaultDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".woca")
}

// Save writes a transcript (one message per JSON line) to path, creating
// parent directories as needed.
func Save(path string, msgs []messages.Message) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	enc := json.NewEncoder(f)
	for _, m := range msgs {
		if err := enc.Encode(m); err != nil {
			return err
		}
	}
	return nil
}

// Load reads a transcript written by Save.
func Load(path string) ([]messages.Message, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []messages.Message
	dec := json.NewDecoder(f)
	for {
		var m messages.Message
		err := dec.Decode(&m)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
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
	defer f.Close()

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
	defer f.Close()

	var s State
	if err := json.NewDecoder(f).Decode(&s); err != nil {
		return State{}, err
	}
	return s, nil
}
