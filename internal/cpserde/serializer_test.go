package cpserde

import (
	"path/filepath"
	"testing"

	lgcheckpoint "github.com/projanvil/langchain-golang/langgraph/checkpoint"
	"github.com/projanvil/langchain-golang/langgraph/checkpoint/savertest"
	lgcpsqlite "github.com/projanvil/langchain-golang/langgraph/checkpoint/sqlite"
)

type testPayload struct {
	Question string
	Options  []string
}

// The upstream sqlite saver must be fully conformant with woca's serializer
// layered in: run the fork's own conformance suite over the pair — the same
// bar the upstream modules clear with the stock serializer.
func TestUpstreamSqliteSaverConformance(t *testing.T) {
	savertest.Run(t, func(t *testing.T) lgcheckpoint.Saver {
		s, err := lgcpsqlite.New(filepath.Join(t.TempDir(), "checkpoints.db"), NewSerializer())
		if err != nil {
			t.Fatalf("sqlite.New: %v", err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return s
	})
}

// RegisterType round-trips a harness struct through the serializer.
func TestRegisterTypeRoundTrip(t *testing.T) {
	RegisterType(testPayload{}, "cpserde.testPayload")
	ser := NewSerializer()

	typ, data, err := ser.DumpsTyped(testPayload{Question: "q", Options: []string{"a", "b"}})
	if err != nil {
		t.Fatal(err)
	}
	if typ != "woca:cpserde.testPayload" {
		t.Fatalf("tag = %q", typ)
	}
	back, err := ser.LoadsTyped(typ, data)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := back.(testPayload)
	if !ok || got.Question != "q" || len(got.Options) != 2 {
		t.Fatalf("round trip = %#v", back)
	}
}

// Unknown structs stay errors — no silent degradation.
func TestUnknownStructStaysError(t *testing.T) {
	type unregistered struct{ X int }
	if _, _, err := NewSerializer().DumpsTyped(unregistered{X: 1}); err == nil {
		t.Fatal("unregistered struct serialized")
	}
}
