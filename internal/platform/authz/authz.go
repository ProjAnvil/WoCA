// Package authz defines the authorization interface and the local
// implementation. Routes declare the scope they require and business code
// checks permission only through the Authorizer interface; remote
// implementations (central policy engines) live in separate packages.
//
// Local mode never blocks: the personal edition authenticates as
// tenancy.Local(), whose unrestricted (nil) scope set grants every scope —
// the code path is identical to server editions, only the policy differs
// (design doc §3.3).
package authz

import (
	"context"
	"errors"
	"slices"

	"github.com/projanvil/woca/internal/platform/tenancy"
)

// ErrForbidden is returned when the authenticated identity lacks the
// required scope. Errors from Authorize other than ErrForbidden indicate
// authorizer malfunction, not denial.
var ErrForbidden = errors.New("authz: forbidden")

// Role names used by the tenant model when issuing credentials (design doc
// §11.2). They are informational labels for humans; authorization decisions
// depend only on the granted scope set.
const (
	RoleAdmin  = "tenant-admin"
	RoleMember = "member"
	RoleViewer = "viewer"
)

// Scopes required by route groups. Every /api route declares exactly one of
// these; new routes must either reuse a scope or add a constant here (and to
// the role sets below) rather than inventing ad-hoc strings.
const (
	ScopeRunsRead      = "runs:read"
	ScopeRunsWrite     = "runs:write"
	ScopeSessionsRead  = "sessions:read"
	ScopeSessionsWrite = "sessions:write"
	ScopeWorkspaceRead = "workspace:read"
	ScopeSettingsRead  = "settings:read"
	ScopeSettingsWrite = "settings:write"
	ScopeMemoryRead    = "memory:read"
	ScopeMemoryWrite   = "memory:write"
	ScopePluginsRead   = "plugins:read"
	ScopePluginsWrite  = "plugins:write"
	ScopeAgentWrite    = "agent:write"
	ScopeModelsRead    = "models:read"
	ScopeModelsWrite   = "models:write"
)

// Authorizer reports whether the tenancy context grants a scope.
//
// Implementations must be safe for concurrent use.
type Authorizer interface {
	Authorize(ctx context.Context, tc tenancy.Context, scope string) error
}

// Local grants scopes from the context itself: a nil Scopes slice means
// unrestricted access (personal mode), otherwise the scope must be listed.
// It has no state and is safe for concurrent use.
type Local struct{}

// NewLocal returns a Local authorizer.
func NewLocal() *Local {
	return &Local{}
}

// Authorize returns nil when tc grants scope, ErrForbidden otherwise.
func (*Local) Authorize(_ context.Context, tc tenancy.Context, scope string) error {
	if tc.HasScope(scope) {
		return nil
	}
	return ErrForbidden
}

// ScopesForRole returns the scope set granted to a role. Credential issuance
// (internal/tenant) materializes scopes from this table so that adding a
// scope updates every role in one place. Unknown roles get no scopes
// (default deny).
func ScopesForRole(role string) []string {
	switch role {
	case RoleAdmin:
		return []string{
			ScopeRunsRead, ScopeRunsWrite,
			ScopeSessionsRead, ScopeSessionsWrite,
			ScopeWorkspaceRead,
			ScopeSettingsRead, ScopeSettingsWrite,
			ScopeMemoryRead, ScopeMemoryWrite,
			ScopePluginsRead, ScopePluginsWrite,
			ScopeAgentWrite,
			ScopeModelsRead, ScopeModelsWrite,
		}
	case RoleMember:
		return []string{
			ScopeRunsRead, ScopeRunsWrite,
			ScopeSessionsRead, ScopeSessionsWrite,
			ScopeWorkspaceRead,
			ScopeMemoryRead, ScopeMemoryWrite,
			ScopeAgentWrite,
			// Model presets: members pick the model their runs use (the
			// credentials stay server-side); authoring presets stays a
			// config-plane (admin) duty.
			ScopeModelsRead, ScopeModelsWrite,
		}
	case RoleViewer:
		return []string{
			ScopeRunsRead,
			ScopeSessionsRead,
			ScopeWorkspaceRead,
			ScopeMemoryRead,
		}
	default:
		return nil
	}
}

// AllScopes lists every scope constant. It exists so tests and future admin
// tooling can assert the role table stays exhaustive.
func AllScopes() []string {
	return slices.Clone(ScopesForRole(RoleAdmin))
}
