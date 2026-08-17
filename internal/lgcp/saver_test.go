// Package lgcp tests: a behavioral contract suite shared by every durable
// Saver (file and SQL), pinning the fork checkpoint.Saver semantics that the
// graph executor relies on — versioned history per thread, parent links,
// write-slot dedup (regular first-write-wins, reserved replace-in-place),
// and typed round-trips of langchain message values.
package lgcp

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/projanvil/langchain-golang/core/messages"
	lgcheckpoint "github.com/projanvil/langchain-golang/langgraph/checkpoint"
	lgtypes "github.com/projanvil/langchain-golang/langgraph/types"

	"github.com/projanvil/woca/internal/wocadb"
)

// runSaverContract asserts the checkpoint.Saver contract against s. Both
// FileSaver and SQLSaver run it; persistence-across-restart is asserted by
// the backend-specific tests on top.
func runSaverContract(t *testing.T, s lgcheckpoint.Saver) {
	t.Helper()
	ctx := context.Background()
	thread := "t1"

	put := func(id, parent string, step int) lgcheckpoint.Config {
		t.Helper()
		cp := lgcheckpoint.Checkpoint{
			V: 1, ID: id, TS: time.Now(),
			ChannelValues:   map[string]any{"messages": []messages.Message{messages.Human("hi " + id)}},
			ChannelVersions: map[string]int64{"messages": int64(step)},
		}
		md := lgcheckpoint.Metadata{Source: "loop", Step: step}
		cfg, err := s.Put(ctx, lgcheckpoint.Config{ThreadID: thread, CheckpointID: parent}, cp, md, map[string]int64{"messages": int64(step)})
		if err != nil {
			t.Fatalf("Put %s: %v", id, err)
		}
		if cfg.CheckpointID != id {
			t.Fatalf("Put returned config %v, want checkpoint id %q", cfg, id)
		}
		return cfg
	}

	// Empty store: latest lookup misses.
	if tup, err := s.GetTuple(ctx, lgcheckpoint.Config{ThreadID: thread}); err != nil || tup != nil {
		t.Fatalf("GetTuple on empty = (%v, %v), want (nil, nil)", tup, err)
	}

	cfg1 := put("cp1", "", 0)
	put("cp2", cfg1.CheckpointID, 1)

	// Latest lookup returns the highest ID.
	tup, err := s.GetTuple(ctx, lgcheckpoint.Config{ThreadID: thread})
	if err != nil || tup == nil {
		t.Fatalf("GetTuple latest: %v %v", tup, err)
	}
	if tup.Checkpoint.ID != "cp2" {
		t.Fatalf("latest = %s, want cp2", tup.Checkpoint.ID)
	}
	if tup.ParentConfig == nil || tup.ParentConfig.CheckpointID != "cp1" {
		t.Fatalf("cp2 parent = %v, want cp1", tup.ParentConfig)
	}
	if tup.Metadata.Step != 1 {
		t.Fatalf("cp2 metadata step = %d, want 1", tup.Metadata.Step)
	}
	// newVersions merged into stored channel versions.
	if got := tup.Checkpoint.ChannelVersions["messages"]; got != 1 {
		t.Fatalf("channel version = %d, want 1", got)
	}
	// Typed value round-trip: []messages.Message survives.
	msgs, ok := tup.Checkpoint.ChannelValues["messages"].([]messages.Message)
	if !ok || len(msgs) != 1 || msgs[0].Content != "hi cp2" {
		t.Fatalf("messages round-trip = %#v", tup.Checkpoint.ChannelValues["messages"])
	}

	// Exact-ID lookup.
	tup, err = s.GetTuple(ctx, lgcheckpoint.Config{ThreadID: thread, CheckpointID: "cp1"})
	if err != nil || tup == nil || tup.Checkpoint.ID != "cp1" {
		t.Fatalf("GetTuple by id: %v %v", tup, err)
	}
	if tup.ParentConfig != nil {
		t.Fatalf("cp1 parent = %v, want nil", tup.ParentConfig)
	}

	// List newest first, with Limit and Before.
	list, err := s.List(ctx, lgcheckpoint.Config{ThreadID: thread}, lgcheckpoint.ListOptions{})
	if err != nil || len(list) != 2 || list[0].Checkpoint.ID != "cp2" || list[1].Checkpoint.ID != "cp1" {
		t.Fatalf("List = %v (%v)", listIDs(list), err)
	}
	list, err = s.List(ctx, lgcheckpoint.Config{ThreadID: thread}, lgcheckpoint.ListOptions{Limit: 1})
	if err != nil || len(list) != 1 || list[0].Checkpoint.ID != "cp2" {
		t.Fatalf("List limit 1 = %v (%v)", listIDs(list), err)
	}
	before := lgcheckpoint.Config{ThreadID: thread, CheckpointID: "cp2"}
	list, err = s.List(ctx, lgcheckpoint.Config{ThreadID: thread}, lgcheckpoint.ListOptions{Before: &before})
	if err != nil || len(list) != 1 || list[0].Checkpoint.ID != "cp1" {
		t.Fatalf("List before cp2 = %v (%v)", listIDs(list), err)
	}

	// Write-slot dedup semantics (mirrors fork MemorySaver / Python
	// put_writes): regular channels keep the first write per (task, idx);
	// reserved channels replace in place.
	cfg2 := lgcheckpoint.Config{ThreadID: thread, CheckpointID: "cp2"}
	mustPutWrites := func(writes []lgcheckpoint.Write, task string) {
		t.Helper()
		if err := s.PutWrites(ctx, cfg2, writes, task, ""); err != nil {
			t.Fatalf("PutWrites %s: %v", task, err)
		}
	}
	mustPutWrites([]lgcheckpoint.Write{{Channel: "messages", Value: []messages.Message{messages.AI("a")}}}, "task1")
	tup, _ = s.GetTuple(ctx, cfg2)
	if len(tup.PendingWrites) != 1 || tup.PendingWrites[0].TaskID != "task1" {
		t.Fatalf("pending writes = %#v", tup.PendingWrites)
	}
	// Same task re-writes a regular slot: ignored.
	mustPutWrites([]lgcheckpoint.Write{{Channel: "messages", Value: []messages.Message{messages.AI("b")}}}, "task1")
	tup, _ = s.GetTuple(ctx, cfg2)
	if got := pendingString(tup, "messages"); got != "a" {
		t.Fatalf("first-write-wins violated: got %q", got)
	}
	// Reserved channel (interrupt): rewrite replaces in place.
	intr := lgtypes.Interrupt{ID: "tools-1", Value: "ask x"}
	mustPutWrites([]lgcheckpoint.Write{{Channel: lgcheckpoint.ReservedInterrupt, Value: intr}}, "task1")
	tup, _ = s.GetTuple(ctx, cfg2)
	if n := countChannel(tup, lgcheckpoint.ReservedInterrupt); n != 1 {
		t.Fatalf("reserved write duplicated: %d entries", n)
	}
	intr2 := lgtypes.Interrupt{ID: "tools-1", Value: "ask x again"}
	mustPutWrites([]lgcheckpoint.Write{{Channel: lgcheckpoint.ReservedInterrupt, Value: intr2}}, "task1")
	tup, _ = s.GetTuple(ctx, cfg2)
	if n := countChannel(tup, lgcheckpoint.ReservedInterrupt); n != 1 {
		t.Fatalf("reserved replace-in-place violated: %d entries", n)
	}
	storedAny := tup.PendingWrites[len(tup.PendingWrites)-1].Value
	if stored, ok := storedAny.(lgtypes.Interrupt); !ok || stored.Value != "ask x again" {
		t.Fatalf("reserved replace-in-place value = %#v", storedAny)
	}

	// PutWrites against a missing checkpoint errors.
	if err := s.PutWrites(ctx, lgcheckpoint.Config{ThreadID: thread, CheckpointID: "missing"}, nil, "t", ""); err == nil {
		t.Fatal("PutWrites on missing checkpoint should error")
	}

	// DeleteThread clears history and writes.
	if err := s.DeleteThread(ctx, thread); err != nil {
		t.Fatalf("DeleteThread: %v", err)
	}
	if tup, err := s.GetTuple(ctx, lgcheckpoint.Config{ThreadID: thread}); err != nil || tup != nil {
		t.Fatalf("GetTuple after delete = (%v, %v), want (nil, nil)", tup, err)
	}
	list, err = s.List(ctx, lgcheckpoint.Config{ThreadID: thread}, lgcheckpoint.ListOptions{})
	if err != nil || len(list) != 0 {
		t.Fatalf("List after delete = %v (%v)", listIDs(list), err)
	}
}

func listIDs(tuples []lgcheckpoint.Tuple) []string {
	out := make([]string, len(tuples))
	for i, t := range tuples {
		out[i] = t.Checkpoint.ID
	}
	return out
}

func pendingString(tup *lgcheckpoint.Tuple, channel string) string {
	for _, w := range tup.PendingWrites {
		if w.Channel == channel {
			if msgs, ok := w.Value.([]messages.Message); ok && len(msgs) > 0 {
				return msgs[0].Content
			}
		}
	}
	return ""
}

func countChannel(tup *lgcheckpoint.Tuple, channel string) int {
	n := 0
	for _, w := range tup.PendingWrites {
		if w.Channel == channel {
			n++
		}
	}
	return n
}

func TestFileSaverContract(t *testing.T) {
	runSaverContract(t, NewFileSaver(t.TempDir()))
}

func TestFileSaverPersistsAcrossReopen(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s1 := NewFileSaver(dir)
	cp := lgcheckpoint.Checkpoint{V: 1, ID: "cp1", TS: time.Now(), ChannelValues: map[string]any{}}
	if _, err := s1.Put(ctx, lgcheckpoint.Config{ThreadID: "th"}, cp, lgcheckpoint.Metadata{Source: "input", Step: -1}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s1.PutWrites(ctx, lgcheckpoint.Config{ThreadID: "th", CheckpointID: "cp1"},
		[]lgcheckpoint.Write{{Channel: lgcheckpoint.ReservedInterrupt, Value: lgtypes.Interrupt{ID: "tools-1"}}}, "task1", ""); err != nil {
		t.Fatal(err)
	}

	s2 := NewFileSaver(dir)
	tup, err := s2.GetTuple(ctx, lgcheckpoint.Config{ThreadID: "th"})
	if err != nil || tup == nil || tup.Checkpoint.ID != "cp1" {
		t.Fatalf("reopen GetTuple = %v %v", tup, err)
	}
	if len(tup.PendingWrites) != 1 || tup.PendingWrites[0].Channel != lgcheckpoint.ReservedInterrupt {
		t.Fatalf("reopen pending writes = %#v", tup.PendingWrites)
	}
	if intr, ok := tup.PendingWrites[0].Value.(lgtypes.Interrupt); !ok || intr.ID != "tools-1" {
		t.Fatalf("interrupt round-trip = %#v", tup.PendingWrites[0].Value)
	}
}

// newSQLite opens a wocadb sqlite handle over a fresh temp file.
func newSQLite(t *testing.T, path string) *wocadb.DB {
	t.Helper()
	db, err := wocadb.Open("sqlite:" + path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestSQLSaverContract(t *testing.T) {
	db := newSQLite(t, filepath.Join(t.TempDir(), "test.db"))
	runSaverContract(t, NewSQLSaver(db.DB, string(db.Dialect)))
}

func TestSQLSaverPersistsAcrossReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "cp.db")
	db1 := newSQLite(t, path)
	cp := lgcheckpoint.Checkpoint{V: 1, ID: "cp9", TS: time.Now(), ChannelValues: map[string]any{}}
	if _, err := NewSQLSaver(db1.DB, string(db1.Dialect)).Put(ctx, lgcheckpoint.Config{ThreadID: "th"}, cp, lgcheckpoint.Metadata{Source: "input", Step: -1}, nil); err != nil {
		t.Fatal(err)
	}
	if err := db1.Close(); err != nil {
		t.Fatal(err)
	}

	db2 := newSQLite(t, path)
	tup, err := NewSQLSaver(db2.DB, string(db2.Dialect)).GetTuple(ctx, lgcheckpoint.Config{ThreadID: "th"})
	if err != nil || tup == nil || tup.Checkpoint.ID != "cp9" {
		t.Fatalf("reopen GetTuple = %v %v", tup, err)
	}
}

func TestFileSaverSanitizesThreadIDs(t *testing.T) {
	ctx := context.Background()
	s := NewFileSaver(t.TempDir())
	cp := lgcheckpoint.Checkpoint{V: 1, ID: "cp1", TS: time.Now(), ChannelValues: map[string]any{}}
	if _, err := s.Put(ctx, lgcheckpoint.Config{ThreadID: "a/../b"}, cp, lgcheckpoint.Metadata{}, nil); err != nil {
		t.Fatal(err)
	}
	tup, err := s.GetTuple(ctx, lgcheckpoint.Config{ThreadID: "a/../b"})
	if err != nil || tup == nil {
		t.Fatalf("thread id with separators round-trip = %v %v", tup, err)
	}
}
