package worker

// Per-run sandbox plumbing (design §4.5/§5.3): a workspace-routed run (EE)
// creates the sandbox under the run's workspace. The executor bind-mounts
// the workspace directory into the container, so there is nothing to move:
// no upload, no download, no sync. woca does not transfer files between
// owners; what the run writes lands on the volume in place.

import (
	"context"
	"fmt"
	"time"

	"github.com/projanvil/woca/internal/ee/nacosrt"
	"github.com/projanvil/woca/internal/platform/executor"
	"github.com/projanvil/woca/internal/platform/executor/providers"
	"github.com/projanvil/woca/internal/runexec"
	"github.com/projanvil/woca/internal/runstate"
)

// provisionRunSandbox sets up the per-run sandbox when the materialized
// config carries an enabled backend (design §4.5). On success it rewrites
// *ctx to carry the run executor and returns the teardown that destroys
// the sandbox. Nil teardown means no backend — the run executes locally,
// exactly the pre-sandbox behavior. Every failure is returned as an error
// so the caller fails the run: no silent local fallback, including on
// sandbox-table read errors.
//
// workspaceID routes the sandbox under the run's workspace: it rides the
// contract-http create body and every request's routing header, and the
// executor maps it to its own workspace_root/<id> directory, which it
// bind-mounts as the container's /workspace (spec §5.3). Runs without a
// workspace identity do not provision a sandbox at all.
func (w *Worker) provisionRunSandbox(ctxp *context.Context, workspaceID string, snap runstate.RunSnapshot) (teardown func(), err error) {
	if w.db == nil || snap.Workspace == "" {
		return nil, nil
	}
	row, err := nacosrt.EnabledSandbox(w.db.DB)
	if err != nil {
		return nil, fmt.Errorf("sandbox: enabled backend read: %w", err)
	}
	if row == nil {
		return nil, nil
	}
	secret := ""
	if w.secretFor != nil {
		secret = w.secretFor(row.ID)
	}
	x, err := providers.New(providers.Config{
		Type:     row.Type,
		Endpoint: row.Endpoint,
		APIKey:   secret,
		Timeout:  time.Duration(row.TimeoutMs) * time.Millisecond,
		// The run's workspace identity (runstate channel, Task 14): rides
		// the contract-http create body and every request's routing header.
		Workspace: workspaceID,
	})
	if err != nil {
		return nil, fmt.Errorf("sandbox: provision backend %q (%s): %w", row.Name, row.Type, err)
	}
	teardown = func() { _ = closeIfClosable(x) }
	// In workspace mode the host workspace path is the shared-volume mount
	// (scheduler and executor see the same path), so host-path translation
	// keeps working — the sandbox works on the same directory the scheduler
	// sees, recorded here so the routing is observable.
	tx := &executor.Translate{Inner: x, HostRoot: snap.Workspace, SandboxRoot: "/workspace"}
	*ctxp = runexec.WithRunExecutor(*ctxp, tx)
	w.log.Info().Str("backend", row.Name).Str("type", row.Type).Msg("run sandbox provisioned")
	return teardown, nil
}

// closeIfClosable destroys a provider sandbox when it supports it.
func closeIfClosable(x executor.Executor) error {
	if c, ok := x.(interface{ Close() error }); ok {
		return c.Close()
	}
	return nil
}
