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

type conflictPayload struct{ A int }

func TestRegisterTypeValidationPanics(t *testing.T) {
	RegisterType(testPayload{}, "cpserde.testPayload") // already registered
	defer func() { t.Log(recover()) }()
	RegisterType(conflictPayload{}, "cpserde.testPayload")
	t.Fatal("duplicate name accepted")
}

func TestRegisterTypeNonStructPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("non-struct accepted")
		}
	}()
	RegisterType(42, "int")
}

func TestRegisterTypeReregisterConflictPanics(t *testing.T) {
	type dual struct{}
	RegisterType(dual{}, "cpserde.dual")
	defer func() {
		if recover() == nil {
			t.Fatal("conflicting re-registration accepted")
		}
	}()
	RegisterType(dual{}, "cpserde.dual-other")
}

func TestLoadsTypedUnknownEnvelopeName(t *testing.T) {
	if _, err := NewSerializer().LoadsTyped("woca:nope", []byte("{}")); err == nil {
		t.Fatal("unknown envelope name decoded")
	}
}

type badPayload struct {
	Ch chan int `json:"ch"`
}

func TestDumpsTypedUnmarshalableFallsBackToError(t *testing.T) {
	// The woca envelope path json.Marshal fails on a chan field — but such
	// a type must be registered to reach it; marshal failure surfaces as an
	// error, never a panic.
	RegisterType(badPayload{}, "cpserde.bad")
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panic: %v", r)
		}
	}()
	if _, _, err := NewSerializer().DumpsTyped(badPayload{Ch: make(chan int)}); err == nil {
		t.Fatal("unmarshalable value serialized")
	}
}
