package lgcp

import (
	"context"
	"testing"
	"time"

	lgcheckpoint "github.com/projanvil/langchain-golang/langgraph/checkpoint"
)

// wocaPayload is a stand-in for agent interrupt payload structs (registered
// via RegisterType at init time in the agent package).
type wocaPayload struct {
	Tool string         `json:"tool"`
	Args map[string]any `json:"args"`
}

func TestWocaSerializerRoundTripRegisteredType(t *testing.T) {
	RegisterType(wocaPayload{}, "lgcp.test.wocaPayload")

	v := wocaPayload{Tool: "Write", Args: map[string]any{"path": "x.txt"}}
	typ, data, err := newSerializer().DumpsTyped(v)
	if err != nil {
		t.Fatalf("DumpsTyped registered type: %v", err)
	}
	back, err := newSerializer().LoadsTyped(typ, data)
	if err != nil {
		t.Fatalf("LoadsTyped: %v", err)
	}
	got, ok := back.(wocaPayload)
	if !ok || got.Tool != "Write" || got.Args["path"] != "x.txt" {
		t.Fatalf("round-trip = %#v", back)
	}
}

func TestWocaSerializerDelegatesRegistryTypes(t *testing.T) {
	typ, data, err := newSerializer().DumpsTyped("plain")
	if err != nil || typ != "json" {
		t.Fatalf("JSON-native passthrough = %q, %v", typ, err)
	}
	back, err := newSerializer().LoadsTyped(typ, data)
	if err != nil || back != "plain" {
		t.Fatalf("passthrough load = %#v, %v", back, err)
	}
}

func TestWocaSerializerRejectsUnknownStructs(t *testing.T) {
	type unknown struct{ X int }
	if _, _, err := newSerializer().DumpsTyped(unknown{1}); err == nil {
		t.Fatal("unknown struct must stay an error (no silent degradation)")
	}
}

// TestSaverPersistsRegisteredTypes pins the integration the agent relies on:
// a durable saver round-trips a registered interrupt payload (reserved
// write) and channel values through the woca-aware serializer.
func TestSaverPersistsRegisteredTypes(t *testing.T) {
	RegisterType(wocaPayload{}, "lgcp.test.wocaPayload")
	ctx := context.Background()
	s := NewFileSaver(t.TempDir())
	cp := lgcheckpoint.Checkpoint{
		V: 1, ID: "cp1", TS: time.Now(),
		ChannelValues: map[string]any{"payload": wocaPayload{Tool: "Bash"}},
	}
	if _, err := s.Put(ctx, lgcheckpoint.Config{ThreadID: "th"}, cp, lgcheckpoint.Metadata{}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.PutWrites(ctx, lgcheckpoint.Config{ThreadID: "th", CheckpointID: "cp1"},
		[]lgcheckpoint.Write{{Channel: lgcheckpoint.ReservedInterrupt, Value: wocaPayload{Tool: "Write", Args: map[string]any{"path": "y.txt"}}}},
		"task1", ""); err != nil {
		t.Fatal(err)
	}

	tup, err := s.GetTuple(ctx, lgcheckpoint.Config{ThreadID: "th"})
	if err != nil || tup == nil {
		t.Fatalf("GetTuple = %v, %v", tup, err)
	}
	cv, ok := tup.Checkpoint.ChannelValues["payload"].(wocaPayload)
	if !ok || cv.Tool != "Bash" {
		t.Fatalf("channel payload = %#v", tup.Checkpoint.ChannelValues["payload"])
	}
	wv, ok := tup.PendingWrites[0].Value.(wocaPayload)
	if !ok || wv.Tool != "Write" || wv.Args["path"] != "y.txt" {
		t.Fatalf("interrupt payload = %#v", tup.PendingWrites[0].Value)
	}
}
