// Package auth contains the narrow seam between the native bootstrap UI and
// the future remote BFF activation contract.
package auth

import (
	"context"
	"errors"
)

// ErrServerContractUnavailable is returned until the remote activation
// endpoint is implemented. It deliberately prevents an API key from being
// repurposed as a bearer credential by the native client.
var ErrServerContractUnavailable = errors.New("native activation endpoint is not available")

type ActivationRequest struct {
	BaseURL string
	APIKey  string
	// BridgeID targets one bridge behind an edge access point ("" for
	// direct connections); it rides the X-Woca-Bridge header.
	BridgeID string
}

type ActivationResult struct {
	Ready     bool
	SessionID string
	ExpiresAt int64
}

type Activator interface {
	Activate(context.Context, ActivationRequest) (ActivationResult, error)
}

type UnavailableActivator struct{}

func (UnavailableActivator) Activate(context.Context, ActivationRequest) (ActivationResult, error) {
	return ActivationResult{}, ErrServerContractUnavailable
}
