// settingswatch propagates management-plane writes to the business roles
// (design §16.3/§16.7): the config service is the sole writer of settings,
// and servers/workers that lost their in-process "PUT → rebuild" hook watch
// a cheap generation fingerprint (two SUMs over settings/secrets updated_at)
// and re-run the build hooks (model, memory engine, debug log) when it
// moves.
//
// Wake-ups come from two sources, fast and safe: the message plane pushes a
// config.dispatch notification the moment the config plane writes (nil wake
// channel = no NATS, push disabled), and a 3s poll remains the correctness
// net for missed notifications and pg-mode deployments. Every applied
// change records a config ack — the row is the service's inventory entry
// and heartbeat for the config plane's status view.
package settingswatch

import (
	"context"
	"time"

	"github.com/projanvil/woca/internal/settings"
	"github.com/projanvil/woca/internal/wocadb"
	"github.com/rs/zerolog"
)

// pollEvery is the fingerprint cadence (the push path only accelerates it).
const pollEvery = 3 * time.Second

// Identity names the watching service for its config ack.
type Identity struct {
	ServiceID string // lease-style identity, e.g. "srv-host-1" / "worker-host-1"
	Role      string // server | worker | scheduler
	Addr      string // optional reachability hint
	// Capabilities is the comma-separated ability set carried on every
	// ack (v1: "sandbox"); the run dispatcher routes by it (design §10.2
	// M-4).
	Capabilities string
}

// Watch polls the settings generation until ctx is cancelled and calls
// onChange after every observed change (not for the initial reading), then
// records the ack. wake, when non-nil, triggers an immediate probe (one
// buffered signal coalesces a burst — the probe re-reads the fingerprint,
// not the payload). st or db nil disables the corresponding half (no
// database: nothing to watch; no ack store: watch without inventory).
func Watch(ctx context.Context, st *settings.Store, db *wocadb.DB, id Identity, log zerolog.Logger, wake <-chan struct{}, onChange func()) {
	if st == nil {
		return
	}
	ack := func(gen string) {
		if db == nil || id.ServiceID == "" {
			return
		}
		if err := db.SetConfigAck(wocadb.ConfigAck{
			ServiceID: id.ServiceID, Role: id.Role, Generation: gen, Addr: id.Addr,
			Capabilities: id.Capabilities, UpdatedAt: wocadb.Now(),
		}); err != nil {
			log.Warn().Err(err).Msg("config ack write failed")
		}
	}
	apply := func(gen string) {
		log.Info().Str("generation", gen).Msg("settings changed via the config plane; hot-swapping")
		onChange()
		ack(gen)
	}
	last := st.Generation()
	log.Info().Str("generation", last).Msg("settings watcher armed")
	// The inventory row exists from the moment the service boots, so the
	// config plane sees every deployed role immediately.
	ack(last)
	ticker := time.NewTicker(pollEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// The ack row doubles as the heartbeat (design §16.7): refresh
			// it every poll so a healthy-but-idle service stays live in the
			// config plane's inventory.
			ack(last)
			if gen := st.Generation(); gen != last {
				last = gen
				apply(gen)
			}
		case <-wake:
			if gen := st.Generation(); gen != last {
				last = gen
				apply(gen)
			}
		}
	}
}
