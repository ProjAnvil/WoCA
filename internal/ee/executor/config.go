package executor

// The executor's config plane (design §10.2 M-4, §17): the materialized
// sandbox_backends table is the parameter authority — the pack carries the
// options, the nacos apply materializes them, and this source caches the
// enabled row so run admission never touches the database. Refresh is the
// OnChange hook's call; between hooks the snapshot is frozen.

import (
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/projanvil/woca/internal/ee/nacosrt"
	"github.com/projanvil/woca/internal/wocadb"
)

// RuntimeParams is the executor's live backend contract: whether a sandbox
// backend is enabled at all, and its Normalize-filled execution options.
// Disabled means zero options — not Normalize defaults, so a consumer can
// never mistake defaults for a configured backend.
type RuntimeParams struct {
	Enabled bool
	Options nacosrt.BackendOptions
}

// paramsState is one immutable cache generation: the served params plus
// the error (if any) from the load that produced them — keep-last-known-
// good means the error outlives the refresh that hit it.
type paramsState struct {
	params RuntimeParams
	err    error
}

// ParamsSource serves RuntimeParams from an in-memory snapshot of the
// materialized enabled-backend row. NewParamsSource loads once; Refresh
// reloads; Snapshot is a lock-free read.
type ParamsSource struct {
	db    *wocadb.DB
	state atomic.Pointer[paramsState]
}

// NewParamsSource loads the initial snapshot from the materialized table
// (empty/disabled → RuntimeParams{Enabled: false}). Read failures leave
// the disabled zero value and surface through LastError.
func NewParamsSource(db *wocadb.DB) *ParamsSource {
	p := &ParamsSource{db: db}
	p.Refresh()
	return p
}

// Snapshot returns the cached params. It never reads the database — only
// Refresh (the pack OnChange hook) moves the value.
func (p *ParamsSource) Snapshot() RuntimeParams {
	return p.state.Load().params
}

// LastError returns the error from the most recent load attempt, if any.
// A failed refresh keeps serving the previous snapshot with its cause
// here, so a wiring problem is observable without a params outage.
func (p *ParamsSource) LastError() error {
	return p.state.Load().err
}

// Refresh reloads the enabled backend row into the cache. On error it
// keeps the last-known-good snapshot (recorded via LastError) rather than
// flipping the executor to disabled over a transient read failure.
func (p *ParamsSource) Refresh() {
	prev := p.state.Load()
	keep := func(err error) {
		next := &paramsState{err: err}
		if prev != nil {
			next.params = prev.params
		}
		p.state.Store(next)
	}
	row, err := nacosrt.EnabledSandbox(p.db.DB)
	if err != nil {
		keep(fmt.Errorf("executor: params refresh: %w", err))
		return
	}
	if row == nil {
		p.state.Store(&paramsState{}) // disabled, zero options
		return
	}
	opts := nacosrt.BackendOptions{}
	if row.Options != nil {
		opts = *row.Options
	}
	if err := opts.Normalize(); err != nil {
		keep(fmt.Errorf("executor: params refresh: %w", err))
		return
	}
	p.state.Store(&paramsState{params: RuntimeParams{Enabled: true, Options: opts}})
}

// AckFunc builds the executor's config-ack body for the heartbeat loop:
// Role=executor, the "sandbox" capability exactly while the live params
// are enabled, the caller-injected in-flight count as Load (design §3.3
// rule 6), and the routable advertise address — the proxy dials ack.Addr
// as a URL (scheme + host required), so a bare host:port is normalized
// to an http:// URL here rather than trusted to the caller; an ack
// without any address is unroutable no matter how fresh. generation/
// active may be nil (zero values); params may be nil (no capability
// ever). ServiceID is stamped here so the ack is complete standalone;
// the heartbeat loop owns UpdatedAt.
func AckFunc(id, advertise string, generation func() string, params *ParamsSource, active func() int64) func() wocadb.ConfigAck {
	if advertise != "" && !strings.Contains(advertise, "://") {
		advertise = "http://" + advertise
	}
	return func() wocadb.ConfigAck {
		ack := wocadb.ConfigAck{ServiceID: id, Role: "executor", Addr: advertise}
		if generation != nil {
			ack.Generation = generation()
		}
		if params != nil && params.Snapshot().Enabled {
			ack.Capabilities = "sandbox"
		}
		if active != nil {
			ack.Load = active()
		}
		return ack
	}
}
