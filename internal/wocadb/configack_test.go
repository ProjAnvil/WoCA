package wocadb

import (
	"path/filepath"
	"testing"
)

// Capabilities ride the config ack (design §10.2 M-4): comma-separated
// ability markers (v1: "sandbox") that the run dispatcher checks before
// queueing sandboxed runs.
func TestConfigAckCapabilitiesRoundTrip(t *testing.T) {
	db, err := Open("sqlite:" + filepath.Join(t.TempDir(), "acks.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	if err := db.SetConfigAck(ConfigAck{
		ServiceID: "worker-1", Role: "worker", Generation: "g1",
		Addr: ":9092", Capabilities: "sandbox", UpdatedAt: Now(),
	}); err != nil {
		t.Fatal(err)
	}
	acks, err := db.ConfigAcks()
	if err != nil {
		t.Fatal(err)
	}
	if len(acks) != 1 {
		t.Fatalf("acks = %d, want 1", len(acks))
	}
	if acks[0].Capabilities != "sandbox" {
		t.Fatalf("capabilities = %q, want sandbox", acks[0].Capabilities)
	}

	// Rewrite without capabilities (a worker that lost its enabled
	// backend clears the marker).
	if err := db.SetConfigAck(ConfigAck{
		ServiceID: "worker-1", Role: "worker", Generation: "g2",
		Addr: ":9092", UpdatedAt: Now(),
	}); err != nil {
		t.Fatal(err)
	}
	acks, err = db.ConfigAcks()
	if err != nil {
		t.Fatal(err)
	}
	if len(acks) != 1 || acks[0].Capabilities != "" || acks[0].Generation != "g2" {
		t.Fatalf("rewritten ack = %+v", acks[0])
	}
}
