// Package lgcp provides woca's durable checkpoint.Saver backends for the
// langchain-golang langgraph runtime: a JSON-file saver and a SQL saver
// (sqlite/postgres via wocadb). Semantics mirror the fork's MemorySaver —
// versioned history per thread, parent links, and Python put_writes
// write-slot dedup — with channel values serialized through the fork's typed
// serde registry so []messages.Message and friends round-trip losslessly.
package lgcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"

	lgcheckpoint "github.com/projanvil/langchain-golang/langgraph/checkpoint"
)

// store is the persistence primitive both backends implement: flat,
// key-addressed record storage the generic saver core builds the Saver
// contract on. Backends serialize records through the shared encode/decode
// helpers and their injected serializer.
type store interface {
	// get returns the record for id (the latest when id is empty), or nil
	// when it does not exist.
	get(ctx context.Context, thread, ns, id string) (*record, error)
	// listIDs returns every checkpoint ID of the thread/namespace.
	listIDs(ctx context.Context, thread, ns string) ([]string, error)
	put(ctx context.Context, thread, ns, id string, rec record) error
	delThread(ctx context.Context, thread string) error
}

// record is one persisted checkpoint plus its pending writes, mirroring the
// fork MemorySaver's stored value (checkpoint, metadata, parent link, writes,
// and the write-slot map that pins put_writes dedup semantics).
type record struct {
	Checkpoint lgcheckpoint.Checkpoint
	Metadata   lgcheckpoint.Metadata
	Parent     *lgcheckpoint.Config
	Writes     []lgcheckpoint.Write
	Slots      []slotRef
}

// slotRef is one entry of the write-slot map, serialized so dedup state
// survives persistence (positions and slot indices are not reconstructible
// from the flat write list alone).
type slotRef struct {
	TaskID string
	Idx    int
	Pos    int
}

// slotKey identifies one write slot: (task ID, write idx), where regular
// channels use their positional idx and reserved channels their fixed
// negative slot (fork MemorySaver semantics).
type slotKey struct {
	taskID string
	idx    int
}

// reservedWriteIdx mirrors the fork's fixed negative slots: rewriting a
// reserved channel replaces in place while regular channels keep the first
// write.
var reservedWriteIdx = map[string]int{
	lgcheckpoint.ReservedError:     -1,
	lgcheckpoint.ReservedInterrupt: -3,
}

// saver is the generic Saver core over a store.
type saver struct {
	mu    sync.Mutex
	back  store
	serde lgcheckpoint.Serializer
}

func newSaver(back store, ser lgcheckpoint.Serializer) *saver {
	return &saver{back: back, serde: ser}
}

// newSerializer is defined in serializer.go (woca-aware wrapper).

// GetTuple implements lgcheckpoint.Saver.
func (s *saver) GetTuple(ctx context.Context, cfg lgcheckpoint.Config) (*lgcheckpoint.Tuple, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.back.get(ctx, cfg.ThreadID, cfg.CheckpointNS, cfg.CheckpointID)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return nil, nil
	}
	tup := rec.tuple(cfg.ThreadID, cfg.CheckpointNS)
	return &tup, nil
}

// List implements lgcheckpoint.Saver: newest (highest ID) first, filtered by
// opts.Before and opts.Filter, capped by opts.Limit.
func (s *saver) List(ctx context.Context, cfg lgcheckpoint.Config, opts lgcheckpoint.ListOptions) ([]lgcheckpoint.Tuple, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids, err := s.back.listIDs(ctx, cfg.ThreadID, cfg.CheckpointNS)
	if err != nil {
		return nil, err
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))
	out := make([]lgcheckpoint.Tuple, 0, len(ids))
	for _, id := range ids {
		if opts.Before != nil && opts.Before.CheckpointID != "" && id >= opts.Before.CheckpointID {
			continue
		}
		rec, err := s.back.get(ctx, cfg.ThreadID, cfg.CheckpointNS, id)
		if err != nil {
			return nil, err
		}
		if rec == nil {
			continue
		}
		if !lgcheckpoint.MetadataMatchesFilter(rec.Metadata, opts.Filter) {
			continue
		}
		out = append(out, rec.tuple(cfg.ThreadID, cfg.CheckpointNS))
		if opts.Limit > 0 && len(out) >= opts.Limit {
			break
		}
	}
	return out, nil
}

// Put implements lgcheckpoint.Saver. The parent link is taken from cfg:
// a non-empty cfg.CheckpointID is recorded as the new checkpoint's parent.
func (s *saver) Put(ctx context.Context, cfg lgcheckpoint.Config, cp lgcheckpoint.Checkpoint, md lgcheckpoint.Metadata, newVersions map[string]int64) (lgcheckpoint.Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored := cp
	if len(newVersions) > 0 {
		if stored.ChannelVersions == nil {
			stored.ChannelVersions = map[string]int64{}
		}
		for k, v := range newVersions {
			stored.ChannelVersions[k] = v
		}
	}
	var parent *lgcheckpoint.Config
	if cfg.CheckpointID != "" {
		c := cfg
		parent = &c
	}
	rec := record{Checkpoint: stored, Metadata: md, Parent: parent}
	if err := s.back.put(ctx, cfg.ThreadID, cfg.CheckpointNS, cp.ID, rec); err != nil {
		return lgcheckpoint.Config{}, err
	}
	return lgcheckpoint.Config{ThreadID: cfg.ThreadID, CheckpointNS: cfg.CheckpointNS, CheckpointID: cp.ID}, nil
}

// PutWrites implements lgcheckpoint.Saver with MemorySaver slot semantics:
// a regular channel occupies its positional idx and keeps the first write;
// a reserved channel occupies its fixed negative slot and is replaced
// in place on rewrite.
func (s *saver) PutWrites(ctx context.Context, cfg lgcheckpoint.Config, writes []lgcheckpoint.Write, taskID, taskPath string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.back.get(ctx, cfg.ThreadID, cfg.CheckpointNS, cfg.CheckpointID)
	if err != nil {
		return err
	}
	if rec == nil {
		return fmt.Errorf("lgcp: PutWrites: no checkpoint %q for thread %q (ns %q)",
			cfg.CheckpointID, cfg.ThreadID, cfg.CheckpointNS)
	}
	slots := map[slotKey]int{}
	for _, sr := range rec.Slots {
		slots[slotKey{taskID: sr.TaskID, idx: sr.Idx}] = sr.Pos
	}
	for i, w := range writes {
		idx := i
		if reserved, ok := reservedWriteIdx[w.Channel]; ok {
			idx = reserved
		}
		key := slotKey{taskID: taskID, idx: idx}
		w.TaskID = taskID
		w.TaskPath = taskPath
		if pos, occupied := slots[key]; occupied {
			if idx >= 0 {
				continue // first write wins
			}
			rec.Writes[pos] = w // reserved slot: replace in place
			continue
		}
		slots[key] = len(rec.Writes)
		rec.Writes = append(rec.Writes, w)
	}
	rec.Slots = rec.Slots[:0]
	for key, pos := range slots {
		rec.Slots = append(rec.Slots, slotRef{TaskID: key.taskID, Idx: key.idx, Pos: pos})
	}
	return s.back.put(ctx, cfg.ThreadID, cfg.CheckpointNS, rec.Checkpoint.ID, *rec)
}

// DeleteThread implements lgcheckpoint.Saver.
func (s *saver) DeleteThread(ctx context.Context, threadID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.back.delThread(ctx, threadID)
}

// tuple assembles the caller-facing Tuple from the record.
func (r *record) tuple(threadID, ns string) lgcheckpoint.Tuple {
	tup := lgcheckpoint.Tuple{
		Config:        lgcheckpoint.Config{ThreadID: threadID, CheckpointNS: ns, CheckpointID: r.Checkpoint.ID},
		Checkpoint:    r.Checkpoint,
		Metadata:      r.Metadata,
		ParentConfig:  r.Parent,
		PendingWrites: append([]lgcheckpoint.Write(nil), r.Writes...),
	}
	return tup
}

// --- wire serialization -------------------------------------------------
//
// The fork's serde registry covers leaf values (messages.Message, Interrupt,
// Send, time.Time, int64, ...); structural maps with int64 values and nested
// any values need per-element encoding, so the wire form serializes each
// typed leaf into {typ, data} pairs and restores them via LoadsTyped.

type wireValue struct {
	Typ  string          `json:"typ"`
	Data json.RawMessage `json:"data"`
}

type wireConfig struct {
	ThreadID     string `json:"thread_id"`
	CheckpointNS string `json:"ns"`
	CheckpointID string `json:"checkpoint_id"`
}

type wirePlannedTask struct {
	ID   string               `json:"id"`
	Node string               `json:"node"`
	Arg  map[string]wireValue `json:"arg,omitempty"`
}

type wireCheckpoint struct {
	V               int                          `json:"v"`
	ID              string                       `json:"id"`
	TS              wireValue                    `json:"ts"`
	ChannelValues   map[string]wireValue         `json:"channel_values"`
	ChannelVersions map[string]string            `json:"channel_versions"`
	VersionsSeen    map[string]map[string]string `json:"versions_seen"`
	Next            []wirePlannedTask            `json:"next,omitempty"`
}

type wireMetadata struct {
	Source   string            `json:"source"`
	Step     int               `json:"step"`
	Parents  map[string]string `json:"parents,omitempty"`
	Counters map[string][2]int `json:"counters,omitempty"`
}

type wireWrite struct {
	TaskID   string    `json:"task_id"`
	TaskPath string    `json:"task_path"`
	Channel  string    `json:"channel"`
	Value    wireValue `json:"value"`
}

type wireRecord struct {
	Checkpoint wireCheckpoint `json:"checkpoint"`
	Metadata   wireMetadata   `json:"metadata"`
	Parent     *wireConfig    `json:"parent,omitempty"`
	Writes     []wireWrite    `json:"writes,omitempty"`
	Slots      []slotRef      `json:"slots,omitempty"`
}

// encodeRecord serializes rec through the typed serde registry.
func encodeRecord(ser lgcheckpoint.Serializer, rec record) ([]byte, error) {
	out := wireRecord{
		Metadata: wireMetadata{Source: rec.Metadata.Source, Step: rec.Metadata.Step, Parents: rec.Metadata.Parents, Counters: rec.Metadata.CountersSinceDeltaSnapshot},
		Writes:   make([]wireWrite, len(rec.Writes)),
		Slots:    rec.Slots,
	}
	if rec.Parent != nil {
		out.Parent = &wireConfig{ThreadID: rec.Parent.ThreadID, CheckpointNS: rec.Parent.CheckpointNS, CheckpointID: rec.Parent.CheckpointID}
	}
	cp := rec.Checkpoint
	out.Checkpoint = wireCheckpoint{
		V:               cp.V,
		ID:              cp.ID,
		ChannelVersions: map[string]string{},
		VersionsSeen:    map[string]map[string]string{},
	}
	ts, data, err := ser.DumpsTyped(cp.TS)
	if err != nil {
		return nil, fmt.Errorf("lgcp: encode ts: %w", err)
	}
	out.Checkpoint.TS = wireValue{Typ: ts, Data: data}
	out.Checkpoint.ChannelValues = make(map[string]wireValue, len(cp.ChannelValues))
	for k, v := range cp.ChannelValues {
		wv, err := encodeValue(ser, v)
		if err != nil {
			return nil, fmt.Errorf("lgcp: encode channel %q: %w", k, err)
		}
		out.Checkpoint.ChannelValues[k] = wv
	}
	for k, v := range cp.ChannelVersions {
		out.Checkpoint.ChannelVersions[k] = strconv.FormatInt(v, 10)
	}
	for node, seen := range cp.VersionsSeen {
		m := map[string]string{}
		for k, v := range seen {
			m[k] = strconv.FormatInt(v, 10)
		}
		out.Checkpoint.VersionsSeen[node] = m
	}
	for _, t := range cp.Next {
		wt := wirePlannedTask{ID: t.ID, Node: t.Node}
		if t.Arg != nil {
			wt.Arg = map[string]wireValue{}
			for k, v := range t.Arg {
				wv, err := encodeValue(ser, v)
				if err != nil {
					return nil, fmt.Errorf("lgcp: encode next-task arg: %w", err)
				}
				wt.Arg[k] = wv
			}
		}
		out.Checkpoint.Next = append(out.Checkpoint.Next, wt)
	}
	for i, w := range rec.Writes {
		wv, err := encodeValue(ser, w.Value)
		if err != nil {
			return nil, fmt.Errorf("lgcp: encode write %q: %w", w.Channel, err)
		}
		out.Writes[i] = wireWrite{TaskID: w.TaskID, TaskPath: w.TaskPath, Channel: w.Channel, Value: wv}
	}
	return json.Marshal(out)
}

// decodeRecord restores a record serialized by encodeRecord.
func decodeRecord(ser lgcheckpoint.Serializer, blob []byte) (record, error) {
	var in wireRecord
	if err := json.Unmarshal(blob, &in); err != nil {
		return record{}, fmt.Errorf("lgcp: decode record: %w", err)
	}
	rec := record{}
	if in.Parent != nil {
		rec.Parent = &lgcheckpoint.Config{ThreadID: in.Parent.ThreadID, CheckpointNS: in.Parent.CheckpointNS, CheckpointID: in.Parent.CheckpointID}
	}
	rec.Metadata = lgcheckpoint.Metadata{
		Source:                     in.Metadata.Source,
		Step:                       in.Metadata.Step,
		Parents:                    in.Metadata.Parents,
		CountersSinceDeltaSnapshot: in.Metadata.Counters,
	}
	cp := lgcheckpoint.Checkpoint{V: in.Checkpoint.V, ID: in.Checkpoint.ID}
	ts, err := ser.LoadsTyped(in.Checkpoint.TS.Typ, in.Checkpoint.TS.Data)
	if err != nil {
		return record{}, fmt.Errorf("lgcp: decode ts: %w", err)
	}
	cp.TS, _ = ts.(time.Time)
	cp.ChannelValues = make(map[string]any, len(in.Checkpoint.ChannelValues))
	for k, wv := range in.Checkpoint.ChannelValues {
		v, err := decodeValue(ser, wv)
		if err != nil {
			return record{}, fmt.Errorf("lgcp: decode channel %q: %w", k, err)
		}
		cp.ChannelValues[k] = v
	}
	cp.ChannelVersions = make(map[string]int64, len(in.Checkpoint.ChannelVersions))
	for k, v := range in.Checkpoint.ChannelVersions {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return record{}, fmt.Errorf("lgcp: decode channel version %q: %w", k, err)
		}
		cp.ChannelVersions[k] = n
	}
	cp.VersionsSeen = make(map[string]map[string]int64, len(in.Checkpoint.VersionsSeen))
	for node, seen := range in.Checkpoint.VersionsSeen {
		m := map[string]int64{}
		for k, v := range seen {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return record{}, fmt.Errorf("lgcp: decode versions-seen %q: %w", k, err)
			}
			m[k] = n
		}
		cp.VersionsSeen[node] = m
	}
	for _, t := range in.Checkpoint.Next {
		pt := lgcheckpoint.PlannedTask{ID: t.ID, Node: t.Node}
		if t.Arg != nil {
			pt.Arg = map[string]any{}
			for k, wv := range t.Arg {
				v, err := decodeValue(ser, wv)
				if err != nil {
					return record{}, fmt.Errorf("lgcp: decode next-task arg: %w", err)
				}
				pt.Arg[k] = v
			}
		}
		cp.Next = append(cp.Next, pt)
	}
	rec.Checkpoint = cp
	rec.Writes = make([]lgcheckpoint.Write, len(in.Writes))
	for i, w := range in.Writes {
		v, err := decodeValue(ser, w.Value)
		if err != nil {
			return record{}, fmt.Errorf("lgcp: decode write %q: %w", w.Channel, err)
		}
		rec.Writes[i] = lgcheckpoint.Write{TaskID: w.TaskID, TaskPath: w.TaskPath, Channel: w.Channel, Value: v}
	}
	rec.Slots = in.Slots
	return rec, nil
}

func encodeValue(ser lgcheckpoint.Serializer, v any) (wireValue, error) {
	typ, data, err := ser.DumpsTyped(v)
	if err != nil {
		return wireValue{}, err
	}
	return wireValue{Typ: typ, Data: data}, nil
}

func decodeValue(ser lgcheckpoint.Serializer, wv wireValue) (any, error) {
	return ser.LoadsTyped(wv.Typ, wv.Data)
}
