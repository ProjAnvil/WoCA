package bridge

// actor.go carries the edge's identity assertion for tunneled traffic
// (product design v1.8 §2.2): in scenario four the EDGE validates the
// client against the private core (login and per-request introspection,
// calling home) and asserts who is calling over the mutually
// authenticated tunnel. The assertion rides as tunnel-payload fields —
// never as headers the client could inject: the edge strips the
// X-Woca-* family from client requests before adding anything of its
// own, and the bridge's tunnel client strips them again on arrival.
//
// The bridge holds zero user credentials (v1.8): this assertion, and the
// core-introspected session mirror for direct connections, are the only
// user identities it ever sees.

import (
	"context"

	"github.com/projanvil/woca/internal/platform/authz"
	"github.com/projanvil/woca/internal/platform/tenancy"
)

// ActorAssertion is one validated caller identity stated by the edge.
type ActorAssertion struct {
	TenantID string `json:"tenant_id"`
	UserID   string `json:"user_id"`
	Remark   string `json:"remark"`
	Role     string `json:"role"`
}

// Context renders the assertion's tenancy context. The business tenant
// maps onto the bridge's trust circle (a stats dimension at the core,
// never an isolation boundary here).
func (a ActorAssertion) Context() tenancy.Context {
	return tenancy.Context{
		TenantID: BridgeTrustTenant,
		UserID:   a.UserID,
		Scopes:   authz.ScopesForRole(a.Role),
	}
}

type actorKey struct{}

// WithActor attaches an edge assertion to a request context.
func WithActor(ctx context.Context, a ActorAssertion) context.Context {
	return context.WithValue(ctx, actorKey{}, a)
}

// ActorFromContext extracts the edge assertion (ok=false when the
// request did not arrive through an authenticated tunnel hop).
func ActorFromContext(ctx context.Context) (ActorAssertion, bool) {
	a, ok := ctx.Value(actorKey{}).(ActorAssertion)
	return a, ok
}
