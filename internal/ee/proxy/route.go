package proxy

import (
	"errors"
	"fmt"
	"net/url"

	"github.com/projanvil/woca/internal/wocadb"
)

// Routing failures. All surface as 503-with-reason: the brief's contract is
// that the caller retries (the conditions clear when an executor acks
// again), never that the proxy guesses a different executor — a workspace's
// files live on its bound executor and nowhere else.
var (
	errMissingWorkspace = errors.New("missing X-Woca-Workspace header: route every sandbox request with it")
	errNoBinding        = errors.New("no workspace binding")
	errBindingDegraded  = errors.New("workspace binding degraded")
	errExecutorNotLive  = errors.New("executor not live")
	errBadExecutorAddr  = errors.New("executor address unparseable")
)

// routeTarget is one resolved forward target: the binding (which carries
// the tenant — never trusted from request headers) and the executor's
// parsed address.
type routeTarget struct {
	binding wocadb.WorkspaceBinding
	url     *url.URL
}

// route resolves a workspace ID to its executor's address:
// binding table → executor's config ack (Role=executor, inside the 15s
// liveness window, routable address). Stale acks fail closed here even
// before the degraded scan flips the row — a workspace is never routed to
// an executor the config plane has not heard from recently.
func (p *Proxy) route(workspaceID string) (routeTarget, error) {
	if p.db == nil {
		return routeTarget{}, fmt.Errorf("proxy has no database: %w", errNoBinding)
	}
	binding, ok, err := p.db.WorkspaceBindingByID(workspaceID)
	if err != nil {
		return routeTarget{}, fmt.Errorf("binding lookup: %w", err)
	}
	if !ok {
		return routeTarget{}, fmt.Errorf("%w for %s (allocate first via POST /internal/bindings)", errNoBinding, workspaceID)
	}
	if binding.State != wocadb.WorkspaceBindingActive {
		return routeTarget{}, fmt.Errorf("%w: workspace %s is degraded (executor %s stopped heartbeating); it recovers automatically once the executor acks again (15s liveness window)", errBindingDegraded, workspaceID, binding.ExecutorID)
	}
	acks, err := p.db.ConfigAcks()
	if err != nil {
		return routeTarget{}, fmt.Errorf("config acks: %w", err)
	}
	now := wocadb.Now()
	var ack wocadb.ConfigAck
	found := false
	for _, a := range acks {
		if a.ServiceID == binding.ExecutorID {
			ack = a
			found = true
		}
	}
	if !found || ack.Role != "executor" || now-ack.UpdatedAt > ackLiveWindow {
		return routeTarget{}, fmt.Errorf("%w: executor %s for workspace %s has no fresh ack (15s window); retry after the executor resumes heartbeating", errExecutorNotLive, binding.ExecutorID, workspaceID)
	}
	if ack.Addr == "" {
		return routeTarget{}, fmt.Errorf("%w: executor %s acked without an address", errExecutorNotLive, binding.ExecutorID)
	}
	u, err := url.Parse(ack.Addr)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return routeTarget{}, fmt.Errorf("%w: executor %s address %q", errBadExecutorAddr, binding.ExecutorID, ack.Addr)
	}
	return routeTarget{binding: binding, url: u}, nil
}
