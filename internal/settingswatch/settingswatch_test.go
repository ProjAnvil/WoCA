package settingswatch

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/projanvil/woca/internal/settings"
	"github.com/rs/zerolog"
)

// A config-plane write (Save) moves the generation and fires the rebuild
// hook; writes that do not change the fingerprint (none here) do not.
func TestWatchFiresOnChange(t *testing.T) {
	dir := t.TempDir()
	st, err := settings.Open("sqlite:" + filepath.Join(dir, "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	var fires atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wake := make(chan struct{}, 1)
	go Watch(ctx, st, st.DB(), Identity{ServiceID: "watch-test", Role: "worker"}, zerolog.Nop(), wake, func() { fires.Add(1) })

	// No change yet: give the loop a moment, expect silence.
	time.Sleep(pollEvery + 50*time.Millisecond)
	if n := fires.Load(); n != 0 {
		t.Fatalf("premature fires = %d", n)
	}

	cfg := st.Load()
	cfg.Model = "watched-model"
	if err := st.Save(cfg); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if fires.Load() >= 1 {
			cancel()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("watcher never fired after a config write")
}

// The ack row carries the service's capabilities (design §10.2 M-4) so
// the run dispatcher can route by ability, not just by liveness.
func TestWatchAcksCapabilities(t *testing.T) {
	dir := t.TempDir()
	st, err := settings.Open("sqlite:" + filepath.Join(dir, "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	ctx := t.Context()
	go Watch(ctx, st, st.DB(), Identity{ServiceID: "cap-worker", Role: "worker", Capabilities: "sandbox"}, zerolog.Nop(), nil, func() {})

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		acks, err := st.DB().ConfigAcks()
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range acks {
			if a.ServiceID == "cap-worker" && a.Capabilities == "sandbox" {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("capabilities never landed in the ack row")
}
