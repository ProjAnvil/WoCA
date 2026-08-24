// apikey.go wires the tenant store into the platform authn interface.
package tenant

import (
	"context"
	"sync/atomic"

	"github.com/projanvil/woca/internal/platform/authn"
	"github.com/projanvil/woca/internal/platform/authz"
	"github.com/projanvil/woca/internal/platform/tenancy"
)

// APIKeyProvider is an authn.Provider backed by the tenant store's api_keys
// table. Unlike the local provider it rejects every request without a valid
// key — including requests with no credential at all — which is the server
// posture (design doc §3.3: same code path, stricter policy).
type APIKeyProvider struct {
	store *Store

	// auths counts authentications served (test/observability aid).
	auths atomic.Int64
}

// NewAPIKeyProvider returns a provider over st.
func NewAPIKeyProvider(st *Store) *APIKeyProvider {
	return &APIKeyProvider{store: st}
}

// Authenticate resolves a woca_ API key to its tenant context. An empty
// credential, unknown prefix, wrong secret, or revoked key yields
// authn.ErrUnauthenticated.
func (p *APIKeyProvider) Authenticate(_ context.Context, credential string) (tenancy.Context, error) {
	p.auths.Add(1)
	k, ok, err := p.store.lookup(credential)
	if err != nil {
		return tenancy.Context{}, err
	}
	if !ok {
		if p.store.revokedKey(credential) {
			return tenancy.Context{}, &authn.Error{Code: authn.CodeRevoked, Err: authn.ErrUnauthenticated}
		}
		return tenancy.Context{}, authn.ErrUnauthenticated
	}
	return tenancy.Context{
		TenantID: k.TenantID,
		UserID:   k.UserID,
		Scopes:   authz.ScopesForRole(k.Role),
	}, nil
}
