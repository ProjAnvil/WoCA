package tenant

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/projanvil/woca/internal/platform/authn"
	"github.com/projanvil/woca/internal/platform/authz"
	"github.com/projanvil/woca/internal/platform/tenancy"
	"github.com/projanvil/woca/internal/wocadb"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	db, err := wocadb.Open("sqlite:" + filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	st, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestTenantCRUD(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	a, err := st.CreateTenant(ctx, "Acme")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(a.ID, "t_") || a.Name != "Acme" {
		t.Fatalf("tenant = %+v", a)
	}
	b, err := st.CreateTenant(ctx, "Beta")
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == b.ID {
		t.Fatal("tenant ids collide")
	}

	ts, err := st.ListTenants(ctx)
	if err != nil || len(ts) != 3 {
		t.Fatalf("list = %v %v (want the seeded local tenant plus both created tenants)", ts, err)
	}
	foundLocal := false
	for _, x := range ts {
		if x.ID == "local" {
			foundLocal = true
		}
	}
	if !foundLocal {
		t.Fatal("migrate did not seed the 'local' machine tenant")
	}

	u, err := st.CreateUser(ctx, a.ID, "alice", authz.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	if u.TenantID != a.ID {
		t.Fatalf("user tenant = %s, want %s", u.TenantID, a.ID)
	}

	// Unknown tenant rejected.
	if _, err := st.CreateUser(ctx, "t_nope", "x", authz.RoleMember); !errors.Is(err, ErrNotFound) {
		t.Fatalf("create user in unknown tenant err = %v, want ErrNotFound", err)
	}
	// Unknown role rejected.
	if _, err := st.CreateUser(ctx, a.ID, "x", "root"); err == nil {
		t.Fatal("unknown role accepted")
	}
}

func TestIssueAndAuthenticate(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	ten, _ := st.CreateTenant(ctx, "Acme")
	u, err := st.CreateUser(ctx, ten.ID, "alice", authz.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	k, full, err := st.IssueKey(ctx, ten.ID, u.ID, "ci", authz.RoleMember)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(full, "woca_"+k.Prefix+"_") || len(full) != len("woca_")+keyPrefixLen+1+keySecretLen {
		t.Fatalf("key material malformed: %q", full)
	}

	p := NewAPIKeyProvider(st)
	tc, err := p.Authenticate(ctx, full)
	if err != nil {
		t.Fatal(err)
	}
	want := tenancy.Context{TenantID: ten.ID, UserID: u.ID, Scopes: authz.ScopesForRole(authz.RoleMember)}
	if tc.TenantID != want.TenantID || tc.UserID != want.UserID {
		t.Fatalf("ctx = %+v, want tenant %s user %s", tc, want.TenantID, want.UserID)
	}
	if len(tc.Scopes) != len(want.Scopes) {
		t.Fatalf("scopes = %v, want %v", tc.Scopes, want.Scopes)
	}
	if !tc.HasScope(authz.ScopeRunsWrite) || tc.HasScope(authz.ScopeSettingsWrite) {
		t.Fatalf("member scopes wrong: %v", tc.Scopes)
	}
}

// wrongSecret returns the key with its final character flipped to a
// different hex digit.
func wrongSecret(key string) string {
	last := key[len(key)-1]
	repl := byte('f')
	if last == 'f' {
		repl = '0'
	}
	return key[:len(key)-1] + string(repl)
}

func TestAuthenticateRejects(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	ten, _ := st.CreateTenant(ctx, "Acme")
	u, _ := st.CreateUser(ctx, ten.ID, "alice", authz.RoleMember)
	_, full, err := st.IssueKey(ctx, ten.ID, u.ID, "ci", authz.RoleMember)
	if err != nil {
		t.Fatal(err)
	}

	p := NewAPIKeyProvider(st)
	bad := []string{
		"",
		"woca_" + strings.Repeat("0", keyPrefixLen) + "_" + strings.Repeat("0", keySecretLen),
		// Wrong secret, valid format. Flip the last hex char to its
		// opposite — a fixed replacement char can equal the original and
		// turn this into the real key (a 1-in-16 flake this test used to
		// have).
		wrongSecret(full),
		"Bearer " + full, // scheme wrapper, not a key
		"ghp_0123456789abcdef",
		"woca_zzz_zzz",
	}
	for _, cred := range bad {
		if _, err := p.Authenticate(ctx, cred); !errors.Is(err, authn.ErrUnauthenticated) {
			t.Errorf("Authenticate(%q) err = %v, want ErrUnauthenticated", cred, err)
		}
	}
}

func TestRevokeKey(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	ten, _ := st.CreateTenant(ctx, "Acme")
	u, _ := st.CreateUser(ctx, ten.ID, "alice", authz.RoleViewer)
	k, full, err := st.IssueKey(ctx, ten.ID, u.ID, "", authz.RoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	// Viewer keys carry viewer scopes only.
	p := NewAPIKeyProvider(st)
	tc, err := p.Authenticate(ctx, full)
	if err != nil {
		t.Fatal(err)
	}
	if tc.HasScope(authz.ScopeRunsWrite) {
		t.Fatal("viewer key grants runs:write")
	}

	if err := st.RevokeKey(ctx, k.Prefix); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Authenticate(ctx, full); !errors.Is(err, authn.ErrUnauthenticated) {
		t.Fatalf("revoked key err = %v, want ErrUnauthenticated", err)
	}
	// Revoking again is a no-op, not an error.
	if err := st.RevokeKey(ctx, k.Prefix); err != nil {
		t.Fatal(err)
	}

	keys, _, err := st.ListKeys(ctx, ten.ID, "", "", "", 200, 0)
	if err != nil || len(keys) != 1 || keys[0].RevokedAt == 0 {
		t.Fatalf("list keys = %v %v", keys, err)
	}
}

func TestKeyIssueValidatesTenantAndUser(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	ten, _ := st.CreateTenant(ctx, "Acme")
	other, _ := st.CreateTenant(ctx, "Beta")
	u, _ := st.CreateUser(ctx, other.ID, "bob", authz.RoleMember)

	if _, _, err := st.IssueKey(ctx, ten.ID, u.ID, "", authz.RoleMember); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant issue err = %v, want ErrNotFound", err)
	}
	if _, _, err := st.IssueKey(ctx, ten.ID, "u_nope", "", authz.RoleMember); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown user issue err = %v, want ErrNotFound", err)
	}
	if _, _, err := st.IssueKey(ctx, ten.ID, u.ID, "", "superuser"); err == nil {
		t.Fatal("unknown role accepted")
	}
}

// TestUniquePrefixes: issuing many keys must never collide in practice and
// every key must authenticate.
func TestUniquePrefixes(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	ten, _ := st.CreateTenant(ctx, "Acme")
	u, _ := st.CreateUser(ctx, ten.ID, "alice", authz.RoleMember)
	p := NewAPIKeyProvider(st)
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		k, full, err := st.IssueKey(ctx, ten.ID, u.ID, "", authz.RoleMember)
		if err != nil {
			t.Fatal(err)
		}
		if seen[k.Prefix] {
			t.Fatalf("prefix %s reused", k.Prefix)
		}
		seen[k.Prefix] = true
		if _, err := p.Authenticate(ctx, full); err != nil {
			t.Fatalf("issued key %d does not authenticate: %v", i, err)
		}
	}
}
