package worker

import (
	"context"
	"time"

	"github.com/projanvil/woca/internal/wocadb"
)

// ackHeartbeatEvery is the capability-ack refresh cadence (design §10.2
// M-4): comfortably inside the dispatcher's 15s live window.
const ackHeartbeatEvery = 10 * time.Second

// AckHeartbeat rewrites this service's config-ack row on a ticker until
// ctx ends. The ack builder supplies the body — role, generation from the
// nacos runtime's last applied pack, capabilities from the live
// enabled-sandbox state, load from the caller's in-flight counter — so the
// same loop serves worker and executor identities. Identity and freshness
// belong to this process, so ServiceID/UpdatedAt are stamped at write time
// whatever the body said. The row doubles as the liveness heartbeat —
// without the refresh, a healthy-but-idle service's ack ages past the
// dispatcher's live window and sandbox-capable runs get rejected.
func (w *Worker) AckHeartbeat(ctx context.Context, every time.Duration, ack func() wocadb.ConfigAck) {
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
			a := wocadb.ConfigAck{}
			if ack != nil {
				a = ack()
			}
			a.ServiceID = w.id
			a.UpdatedAt = wocadb.Now()
			if err := w.db.SetConfigAck(a); err != nil {
				w.log.Warn().Err(err).Msg("capability ack heartbeat failed")
			}
		}
	}
}
