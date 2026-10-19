package worker

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/projanvil/woca/internal/wocadb"
	"github.com/rs/zerolog"
)

// The capability heartbeat (design §10.2 M-4): the worker rewrites its
// config-ack row on a short ticker so the 15s live window never silently
// prunes a healthy-but-idle sandbox-capable worker.
func TestWorkerAckHeartbeat(t *testing.T) {
	db, err := wocadb.Open("sqlite:" + filepath.Join(t.TempDir(), "hb.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := db.SetConfigAck(wocadb.ConfigAck{
		ServiceID: "hb-worker", Role: "worker", Generation: "g0", UpdatedAt: wocadb.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	w := &Worker{
		id:  "hb-worker",
		log: zerolog.Nop(),
		db:  db,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.AckHeartbeat(ctx, 10*time.Millisecond,
		func() string { return "g9" },
		func() string { return "sandbox" },
	)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		acks, err := db.ConfigAcks()
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range acks {
			if a.ServiceID == "hb-worker" && a.Generation == "g9" && a.Capabilities == "sandbox" {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("heartbeat never rewrote the ack row")
}
