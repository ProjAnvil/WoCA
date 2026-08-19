// Package tenancy defines the tenant context shared by all platform
// interfaces. Business code reads tenant identity only through this
// package; local and remote implementations inject it the same way.
package tenancy

import (
	"context"
	"slices"
)

// Context carries the tenant identity of the current request.
type Context struct {
	// TenantID identifies the tenant.
	TenantID string
	// OrgID identifies the organization within the tenant.
	OrgID string
	// UserID identifies the acting user.
	UserID string
	// Scopes lists granted permission scopes. A nil slice means
	// unrestricted access.
	Scopes []string
}

// contextKey is an unexported key type to avoid collisions with other
// packages storing values in a context.
type contextKey struct{}

// NewContext returns a child context carrying tc.
func NewContext(ctx context.Context, tc Context) context.Context {
	return context.WithValue(ctx, contextKey{}, tc)
}

// FromContext extracts the tenant Context injected by NewContext.
// It returns the zero Context and false when none was injected.
func FromContext(ctx context.Context) (Context, bool) {
	tc, ok := ctx.Value(contextKey{}).(Context)
	return tc, ok
}

// HasScope reports whether scope is granted. A nil Scopes slice means
// unrestricted access, so every scope is granted.
func (c Context) HasScope(scope string) bool {
	if c.Scopes == nil {
		return true
	}
	return slices.Contains(c.Scopes, scope)
}

// IsZero reports whether c carries no identity. Scopes alone do not
// make a Context non-zero.
func (c Context) IsZero() bool {
	return c.TenantID == "" && c.OrgID == "" && c.UserID == ""
}

// Local returns the default Context for single-node (personal) mode:
// tenant and user are both "local" and scopes are unrestricted.
func Local() Context {
	return Context{TenantID: "local", UserID: "local"}
}
