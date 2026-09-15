package tenancy

import (
	"slices"
	"testing"
)

func TestNewContextFromContextRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		in   Context
	}{
		{name: "full", in: Context{TenantID: "t1", OrgID: "o1", UserID: "u1", Scopes: []string{"read", "write"}}},
		{name: "nil scopes", in: Context{TenantID: "t2", UserID: "u2"}},
		{name: "zero value", in: Context{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := NewContext(t.Context(), tt.in)
			got, ok := FromContext(ctx)
			if !ok {
				t.Fatal("FromContext should report injected context")
			}
			if got.TenantID != tt.in.TenantID || got.OrgID != tt.in.OrgID || got.UserID != tt.in.UserID {
				t.Fatalf("round trip mismatch: got %+v, want %+v", got, tt.in)
			}
			if !slices.Equal(got.Scopes, tt.in.Scopes) {
				t.Fatalf("scopes mismatch: got %v, want %v", got.Scopes, tt.in.Scopes)
			}
		})
	}
}

func TestFromContextNotInjected(t *testing.T) {
	got, ok := FromContext(t.Context())
	if ok {
		t.Fatal("FromContext should return false when nothing was injected")
	}
	if !got.IsZero() {
		t.Fatalf("expected zero Context, got %+v", got)
	}
}

func TestHasScope(t *testing.T) {
	tests := []struct {
		name   string
		scopes []string
		scope  string
		want   bool
	}{
		{name: "present", scopes: []string{"read", "write"}, scope: "read", want: true},
		{name: "absent", scopes: []string{"read"}, scope: "admin", want: false},
		{name: "nil scopes are unrestricted", scopes: nil, scope: "anything", want: true},
		{name: "empty query", scopes: []string{"read"}, scope: "", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := Context{Scopes: tt.scopes}
			if got := c.HasScope(tt.scope); got != tt.want {
				t.Fatalf("HasScope(%q) = %v, want %v", tt.scope, got, tt.want)
			}
		})
	}
}

func TestIsZero(t *testing.T) {
	tests := []struct {
		name string
		in   Context
		want bool
	}{
		{name: "zero", in: Context{}, want: true},
		{name: "tenant set", in: Context{TenantID: "t"}, want: false},
		{name: "org set", in: Context{OrgID: "o"}, want: false},
		{name: "user set", in: Context{UserID: "u"}, want: false},
		{name: "scopes only still zero", in: Context{Scopes: []string{"read"}}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.in.IsZero(); got != tt.want {
				t.Fatalf("IsZero() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestLocal(t *testing.T) {
	c := Local()
	if c.TenantID != "local" {
		t.Fatalf("TenantID = %q, want %q", c.TenantID, "local")
	}
	if c.UserID != "local" {
		t.Fatalf("UserID = %q, want %q", c.UserID, "local")
	}
	if c.OrgID != "" {
		t.Fatalf("OrgID = %q, want empty", c.OrgID)
	}
	if c.Scopes != nil {
		t.Fatalf("Scopes = %v, want nil (unrestricted)", c.Scopes)
	}
	if c.IsZero() {
		t.Fatal("Local() context should not be zero")
	}
	if !c.HasScope("any.scope.at.all") {
		t.Fatal("Local() with nil scopes should permit any scope")
	}
}
