// Package providers builds sandbox executors from resolved connection
// profiles. It is the single dispatch point mapping a sandbox backend
// type (the sandbox_backends rows of the management console) to a
// concrete executor.Executor implementation.
package providers

import (
	"fmt"
	"net/url"
	"time"

	"github.com/projanvil/woca/internal/platform/executor"
	"github.com/projanvil/woca/internal/platform/executor/providers/cfbridge"
)

// Config is the resolved connection profile for one sandbox backend.
type Config struct {
	// Type selects the backend adapter: contract-http, cfbridge, e2b,
	// daytona, modal, microsandbox, sidecar. The type enum is the full
	// contract set; only a subset is registered at runtime.
	Type     string
	Endpoint string
	APIKey   string
	Timeout  time.Duration
	// Workspace is the optional workspace ID the sandbox is routed under
	// (EE sandbox plane): the contract-http create body's workspace field
	// plus the X-Woca-Workspace routing header on every request. Empty =
	// not workspace-routed (personal edition / transition); backends
	// without workspace routing (cfbridge) ignore it.
	Workspace string
}

// New builds an Executor for the backend type. Unknown or unregistered
// types error; the endpoint must be an absolute URL before any provider
// is constructed.
func New(cfg Config) (executor.Executor, error) {
	if u, err := url.Parse(cfg.Endpoint); err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("providers: invalid endpoint %q", cfg.Endpoint)
	}
	switch cfg.Type {
	case "contract-http":
		return executor.NewSandbox(executor.SandboxConfig{
			Endpoint:  cfg.Endpoint,
			APIKey:    cfg.APIKey,
			Timeout:   cfg.Timeout,
			Workspace: cfg.Workspace,
		})
	case "cfbridge":
		return cfbridge.New(cfbridge.Config{
			Endpoint: cfg.Endpoint,
			APIKey:   cfg.APIKey,
			Timeout:  cfg.Timeout,
		})
	default:
		return nil, fmt.Errorf("providers: sandbox type %q not available", cfg.Type)
	}
}
