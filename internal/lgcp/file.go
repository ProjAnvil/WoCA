package lgcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	lgcheckpoint "github.com/projanvil/langchain-golang/langgraph/checkpoint"
)

// FileSaver persists checkpoints as JSON files under a directory:
// <dir>/<thread>/<ns>/<checkpoint-id>.json. It implements the fork's
// checkpoint.Saver over the shared saver core.
type FileSaver struct {
	core *saver
	dir  string
}

// Saver delegation.

// GetTuple implements lgcheckpoint.Saver.
func (f *FileSaver) GetTuple(ctx context.Context, cfg lgcheckpoint.Config) (*lgcheckpoint.Tuple, error) {
	return f.core.GetTuple(ctx, cfg)
}

// List implements lgcheckpoint.Saver.
func (f *FileSaver) List(ctx context.Context, cfg lgcheckpoint.Config, opts lgcheckpoint.ListOptions) ([]lgcheckpoint.Tuple, error) {
	return f.core.List(ctx, cfg, opts)
}

// Put implements lgcheckpoint.Saver.
func (f *FileSaver) Put(ctx context.Context, cfg lgcheckpoint.Config, cp lgcheckpoint.Checkpoint, md lgcheckpoint.Metadata, newVersions map[string]int64) (lgcheckpoint.Config, error) {
	return f.core.Put(ctx, cfg, cp, md, newVersions)
}

// PutWrites implements lgcheckpoint.Saver.
func (f *FileSaver) PutWrites(ctx context.Context, cfg lgcheckpoint.Config, writes []lgcheckpoint.Write, taskID, taskPath string) error {
	return f.core.PutWrites(ctx, cfg, writes, taskID, taskPath)
}

// DeleteThread implements lgcheckpoint.Saver.
func (f *FileSaver) DeleteThread(ctx context.Context, threadID string) error {
	return f.core.DeleteThread(ctx, threadID)
}

// NewFileSaver creates a file-backed Saver storing under dir (created on
// demand).
func NewFileSaver(dir string) *FileSaver {
	ser := newSerializer()
	return &FileSaver{core: newSaver(&fileStore{dir: dir, ser: ser}, ser), dir: dir}
}

// fileStore is the file-backed store primitive.
type fileStore struct {
	dir string
	ser lgcheckpoint.Serializer
}

func (f *fileStore) get(ctx context.Context, thread, ns, id string) (*record, error) {
	dir := f.nsDir(thread, ns)
	if id == "" {
		entries, err := listJSON(dir)
		if err != nil || len(entries) == 0 {
			return nil, err
		}
		latest := entries[0]
		for _, e := range entries[1:] {
			if e > latest {
				latest = e
			}
		}
		id = latest
	}
	rec, err := f.read(dir, id)
	if err != nil || rec == nil {
		return nil, err
	}
	return rec, nil
}

func (f *fileStore) listIDs(ctx context.Context, thread, ns string) ([]string, error) {
	entries, err := listJSON(f.nsDir(thread, ns))
	return entries, err
}

func (f *fileStore) put(ctx context.Context, thread, ns, id string, rec record) error {
	dir := f.nsDir(thread, ns)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	blob, err := encodeRecord(f.ser, rec)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, sanitizeKey(id)+".json"), blob, 0o644)
}

func (f *fileStore) delThread(ctx context.Context, thread string) error {
	return os.RemoveAll(filepath.Join(f.dir, sanitizeKey(thread)))
}

func (f *fileStore) nsDir(thread, ns string) string {
	return filepath.Join(f.dir, sanitizeKey(thread), sanitizeKey(ns))
}

func (f *fileStore) read(dir, id string) (*record, error) {
	blob, err := os.ReadFile(filepath.Join(dir, sanitizeKey(id)+".json"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	rec, err := decodeRecord(f.ser, blob)
	if err != nil {
		return nil, err
	}
	return &rec, nil
}

// listJSON returns the checkpoint IDs (file names without extension) present
// in dir; an empty slice when dir does not exist yet.
func listJSON(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		ids = append(ids, strings.TrimSuffix(e.Name(), ".json"))
	}
	return ids, nil
}

// sanitizeKey replaces characters unsafe in a file name so any thread,
// namespace, or checkpoint ID can be used as a path component.
func sanitizeKey(key string) string {
	if key == "" {
		return "_"
	}
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
