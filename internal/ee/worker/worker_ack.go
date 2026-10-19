package worker

import (
	"context"
	"time"

	"github.com/projanvil/woca/internal/wocadb"
)

// ackHeartbeatEvery is the capability-ack refresh cadence (design §10.2
// M-4): comfortably inside the dispatcher's 15s live window.
const ackHeartbeatEvery = 10 * time.Second

// AckHeartbeat rewrites this worker's config-ack row on a ticker until
// ctx ends: the generation comes from the nacos runtime's last applied
// pack, the capabilities from the live enabled-sandbox state. The row
// doubles as the liveness heartbeat — without the refresh, a
// healthy-but-idle worker's ack ages past the dispatcher's live window
// and sandbox-capable runs get rejected.
func (w *Worker) AckHeartbeat(ctx context.Context, every time.Duration, generation func() string, capabilities func() string) {
	if every <= 0 {
		every = ackHeartbeatEvery
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			caps := ""
			if capabilities != nil {
				caps = capabilities()
			}
			gen := ""
			if generation != nil {
				gen = generation()
			}
			if err := w.db.SetConfigAck(wocadb.ConfigAck{
				ServiceID: w.id, Role: "worker", Generation: gen,
				Capabilities: caps, UpdatedAt: wocadb.Now(),
			}); err != nil {
				w.log.Warn().Err(err).Msg("capability ack heartbeat failed")
			}
		}
	}
}
