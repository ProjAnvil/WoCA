package graph

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// FileCheckpointer persists checkpoints to a directory as JSON files keyed by
// the checkpoint key (the key is used as the filename, sanitized).
type FileCheckpointer struct {
	dir string
}

// NewFileCheckpointer creates a checkpointer that stores checkpoints as JSON
// files under dir. The directory is created on demand.
func NewFileCheckpointer(dir string) *FileCheckpointer {
	return &FileCheckpointer{dir: dir}
}

// checkpointFile is the on-disk JSON representation of a checkpoint.
type checkpointFile struct {
	Node   string `json:"node"`
	Values State  `json:"values"`
}

// Put implements Checkpointer.
func (f *FileCheckpointer) Put(key string, cp checkpoint) error {
	if err := os.MkdirAll(f.dir, 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(checkpointFile{Node: cp.node, Values: cp.values})
	if err != nil {
		return err
	}
	return os.WriteFile(f.path(key), data, 0o644)
}

// Get implements Checkpointer.
func (f *FileCheckpointer) Get(key string) (checkpoint, bool) {
	data, err := os.ReadFile(f.path(key))
	if err != nil {
		return checkpoint{}, false
	}
	var cf checkpointFile
	if err := json.Unmarshal(data, &cf); err != nil {
		return checkpoint{}, false
	}
	return checkpoint{node: cf.Node, values: cf.Values}, true
}

// path resolves a checkpoint key to the file that stores it.
func (f *FileCheckpointer) path(key string) string {
	return filepath.Join(f.dir, sanitizeKey(key)+".json")
}

// sanitizeKey replaces characters that are unsafe in a file name with an
// underscore so that any checkpoint key can be used as a file name.
func sanitizeKey(key string) string {
	var b strings.Builder
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// newCheckpointKey returns a unique, file-name-safe key for a new checkpoint.
func newCheckpointKey() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand.Read is not expected to fail; fall back to a timestamp
		// to keep the function total without widening the import surface.
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
