// Package authn defines the authentication provider interface and local
// implementations. Business code programs against the Provider interface
// only; remote implementations (OIDC, gRPC token verification, ...) live
// in separate packages.
package authn

import (
	"context"
	"crypto/subtle"
	"errors"

	"github.com/projanvil/woca/internal/platform/tenancy"
)

// ErrUnauthenticated is returned when a credential cannot be authenticated.
var ErrUnauthenticated = errors.New("authn: unauthenticated")

// Provider authenticates a credential and returns the tenancy context it
// maps to. The credential is a Bearer token or API key; an empty string
// means no credential was presented.
//
// Implementations must be safe for concurrent use.
type Provider interface {
	Authenticate(ctx context.Context, credential string) (tenancy.Context, error)
}

// Local is a Provider that trusts the local machine: any credential,
// including an empty one, authenticates as the local user. This is the
// personal-edition behavior — the policy is relaxed but the code path is
// identical to stricter providers.
//
// Local has no state and is safe for concurrent use.
type Local struct{}

// NewLocal returns a Local provider.
func NewLocal() *Local {
	return &Local{}
}

// Authenticate always succeeds, returning tenancy.Local().
func (*Local) Authenticate(_ context.Context, _ string) (tenancy.Context, error) {
	return tenancy.Local(), nil
}

// Static is a Provider that authenticates a single pre-shared token. It is
// intended for tests and single-machine password scenarios.
//
// Static is immutable after construction and safe for concurrent use.
type Static struct {
	token string
	tc    tenancy.Context
}

// NewStatic returns a Static provider that maps the given token to the
// given tenancy context. The context is copied defensively.
func NewStatic(token string, tc tenancy.Context) *Static {
	return &Static{token: token, tc: cloneContext(tc)}
}

// Authenticate returns the configured tenancy context when credential
// matches the configured token, and ErrUnauthenticated otherwise. The
// returned context is a defensive copy so callers cannot mutate provider
// state.
func (s *Static) Authenticate(_ context.Context, credential string) (tenancy.Context, error) {
	if credential == "" || subtle.ConstantTimeCompare([]byte(credential), []byte(s.token)) != 1 {
		return tenancy.Context{}, ErrUnauthenticated
	}
	return cloneContext(s.tc), nil
}

// cloneContext copies tc, including its Scopes slice.
func cloneContext(tc tenancy.Context) tenancy.Context {
	out := tc
	if tc.Scopes != nil {
		out.Scopes = append([]string(nil), tc.Scopes...)
	}
	return out
}
