package relay

import (
	"context"
	"sync"
	"time"
)

// Revoker performs the revocation cascade (design §5): wake in-flight
// polls (instant 410), drop the generation lease, TOMBSTONE the key's
// sessions (see Revoke below — rows stay so post-revoke requests resolve
// to code "revoked", the SPA's revoked-notice contract), drain parked
// requests with an error, and audit the event. It also sweeps the key
// table so a revoke issued by a separate `woca-relay key revoke` process
// takes effect in the running relay within the sweep interval.
type Revoker struct {
	keys  *KeyStore
	gens  *GenerationRegistry
	park  *ParkingQueue
	audit *AuditLog

	mu   sync.Mutex
	done map[int64]bool
}

// NewRevoker builds the cascade coordinator.
func NewRevoker(keys *KeyStore, gens *GenerationRegistry, park *ParkingQueue, audit *AuditLog) *Revoker {
	return &Revoker{keys: keys, gens: gens, park: park, audit: audit, done: map[int64]bool{}}
}

// Revoke runs the cascade for one already-revoked key row. Idempotent.
func (rv *Revoker) Revoke(ctx context.Context, k Key) {
	rv.mu.Lock()
	if rv.done[k.ID] {
		rv.mu.Unlock()
		return
	}
	rv.done[k.ID] = true
	rv.mu.Unlock()

	// Bridge side first: in-flight polls return 410 immediately (design
	// §5.1), then the lease drops so no generation resurrects.
	rv.gens.CancelPolls(k.ID, ErrRevoked)
	rv.gens.Drop(k.ID)
	// Client side: sessions are TOMBSTONED, not deleted (design §5.2's
	// "delete" is about denying access, not about erasing rows):
	// ResolveSession's keys join already returns ErrKeyRevoked before any
	// expiry check, so every subsequent request with a revoked session —
	// cookie or Bearer — determinably resolves to 401 {"code":"revoked"}
	// and the SPA's revoked notice fires (WocaApp.vue:137). Deleting the
	// rows would make those requests indistinguishable from logged-out
	// (401 "unauthorized") and the notice would never fire.
	//
	// Tombstoned rows are inert and bounded: they can never authenticate,
	// and ResolveSession already deletes any session row it finds expired
	// (the 7d sliding / 30d absolute check runs for live keys). A lazy
	// purge of expired tombstones rides the same path — a revoked key's
	// session rows disappear on their last expiry touch or can be reaped
	// by a future `DELETE FROM sessions WHERE touched_at < ?` maintenance
	// sweep; neither is required for correctness.
	// Parked but unpicked requests release with an error (design §5.3);
	// in-flight streams get the fail cause (the auth_revoked injection).
	rv.park.FailAll(k.ID, ErrRevoked)
	rv.audit.Event(AuditRevocation, k.Name, "key revoked")
}

// Sweep cascades every revoked key not yet handled. The CLI writes
// revoked_at; the running relay observes it here.
func (rv *Revoker) Sweep(ctx context.Context) error {
	revoked, err := rv.keys.ListRevokedKeys(ctx)
	if err != nil {
		return err
	}
	for _, k := range revoked {
		rv.Revoke(ctx, k)
	}
	return nil
}

// Watch sweeps on interval until ctx ends (serve wires 1s; tests drive
// Sweep directly for determinism).
func (rv *Revoker) Watch(ctx context.Context, interval time.Duration) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			_ = rv.Sweep(ctx)
		}
	}
}
