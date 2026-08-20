package authz

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/projanvil/woca/internal/platform/tenancy"
)

func TestLocal(t *testing.T) {
	a := NewLocal()
	if a == nil {
		t.Fatal("NewLocal() returned nil")
	}
	var _ Authorizer = a

	tests := []struct {
		name    string
		tc      tenancy.Context
		scope   string
		wantErr bool
	}{
		{
			name:  "nil scopes grants everything (personal mode)",
			tc:    tenancy.Local(),
			scope: ScopeSettingsWrite,
		},
		{
			name:  "granted scope",
			tc:    tenancy.Context{TenantID: "t", Scopes: []string{ScopeRunsRead, ScopeRunsWrite}},
			scope: ScopeRunsWrite,
		},
		{
			name:    "missing scope",
			tc:      tenancy.Context{TenantID: "t", Scopes: []string{ScopeRunsRead}},
			scope:   ScopeRunsWrite,
			wantErr: true,
		},
		{
			name:    "empty scope set denies",
			tc:      tenancy.Context{TenantID: "t", Scopes: []string{}},
			scope:   ScopeRunsRead,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := a.Authorize(context.Background(), tt.tc, tt.scope)
			if tt.wantErr {
				if !errors.Is(err, ErrForbidden) {
					t.Fatalf("Authorize error = %v, want ErrForbidden", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Authorize returned error: %v", err)
			}
		})
	}
}

func TestScopesForRole(t *testing.T) {
	admin := ScopesForRole(RoleAdmin)
	member := ScopesForRole(RoleMember)
	viewer := ScopesForRole(RoleViewer)

	// Admin must cover every scope: AllScopes is defined as the admin set,
	// so the interesting assertion is that member and viewer are subsets.
	adminSet := make(map[string]bool, len(admin))
	for _, s := range admin {
		adminSet[s] = true
	}
	for _, s := range member {
		if !adminSet[s] {
			t.Errorf("member scope %q missing from admin set", s)
		}
	}
	for _, s := range viewer {
		if !adminSet[s] {
			t.Errorf("viewer scope %q missing from admin set", s)
		}
	}
	// Viewer must not hold any write scope.
	for _, s := range viewer {
		if strings.HasSuffix(s, ":write") {
			t.Errorf("viewer role grants write scope %q", s)
		}
	}
	if got := ScopesForRole("no-such-role"); got != nil {
		t.Errorf("ScopesForRole(unknown) = %v, want nil (default deny)", got)
	}
	// Returned slices must be safe to mutate by callers.
	member[0] = "mutated"
	if reflect.DeepEqual(ScopesForRole(RoleMember), member) {
		t.Error("ScopesForRole returns shared backing array across calls")
	}
}

func TestAllScopesClone(t *testing.T) {
	all := AllScopes()
	all[0] = "mutated"
	if AllScopes()[0] == "mutated" {
		t.Error("AllScopes() returns shared backing array")
	}
}
