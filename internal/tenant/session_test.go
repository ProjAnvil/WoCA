package tenant

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/projanvil/woca/internal/platform/authz"
	"github.com/projanvil/woca/internal/wocadb"
)

func TestIssueSessionReturnsFullIDOnce(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	tenantID, userID := seedSessionTenant(t, st)

	sess, full, err := st.IssueSession(ctx, tenantID, userID, "device-1", "thumbprint-1", authz.RoleMember, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(full, "wsess_") {
		t.Fatalf("full session id = %q, want wsess_ prefix", full)
	}
	if sess.Prefix == "" || len(sess.Prefix) != sessionPrefixLen {
		t.Fatalf("prefix = %q", sess.Prefix)
	}
	if sess.DeviceID != "device-1" || sess.DPoPThumbprint != "thumbprint-1" {
		t.Fatalf("session = %+v", sess)
	}
	if sess.RevokedAt != 0 {
		t.Fatalf("new session is revoked: %+v", sess)
	}

	got, ok, err := st.LookupSession(ctx, full)
	if err != nil || !ok {
		t.Fatalf("LookupSession = ok=%v err=%v", ok, err)
	}
	if got.TenantID != tenantID || got.UserID != userID || got.DeviceID != "device-1" {
		t.Fatalf("lookup = %+v", got)
	}
}

func TestLookupSessionRejectsWrongSecretAndMalformed(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	tenantID, userID := seedSessionTenant(t, st)

	_, full, err := st.IssueSession(ctx, tenantID, userID, "device-1", "tp", authz.RoleMember, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	for _, bad := range []string{
		"",
		"wsess_" + strings.Repeat("0", 12) + "_" + strings.Repeat("0", 48),
		"Bearer abc",
		full[:len(full)-1],    // truncated secret
		"wsess_" + full,       // double prefix
		strings.ToUpper(full), // non-hex (uppercase is rejected)
	} {
		if _, ok, err := st.LookupSession(ctx, bad); ok || err != nil {
			t.Fatalf("LookupSession(%q) = ok=%v err=%v, want ok=false err=nil", bad, ok, err)
		}
	}
}

func TestLookupSessionRejectsExpiredSession(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	tenantID, userID := seedSessionTenant(t, st)

	_, full, err := st.IssueSession(ctx, tenantID, userID, "device-1", "tp", authz.RoleMember, -time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := st.LookupSession(ctx, full); ok || err != nil {
		t.Fatalf("expired session authenticates: ok=%v err=%v", ok, err)
	}
}

func TestRevokeSessionDisablesLookup(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	tenantID, userID := seedSessionTenant(t, st)

	_, full, err := st.IssueSession(ctx, tenantID, userID, "device-1", "tp", authz.RoleMember, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.RevokeSession(ctx, full); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := st.LookupSession(ctx, full); ok || err != nil {
		t.Fatalf("revoked session authenticates: ok=%v err=%v", ok, err)
	}
	// Revoking an unknown session is not an error.
	if err := st.RevokeSession(ctx, "wsess_"+strings.Repeat("ab", 6)+"_"+strings.Repeat("cd", 24)); err != nil {
		t.Fatalf("revoke unknown = %v", err)
	}
}

func TestTouchSessionUpdatesLastValidated(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	tenantID, userID := seedSessionTenant(t, st)

	sess, full, err := st.IssueSession(ctx, tenantID, userID, "device-1", "tp", authz.RoleMember, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if sess.LastValidatedAt != 0 {
		t.Fatalf("LastValidatedAt = %d, want 0 at issue", sess.LastValidatedAt)
	}
	if err := st.TouchSession(ctx, full, 12345); err != nil {
		t.Fatal(err)
	}
	got, ok, err := st.LookupSession(ctx, full)
	if err != nil || !ok {
		t.Fatal(err)
	}
	if got.LastValidatedAt != 12345 {
		t.Fatalf("LastValidatedAt = %d, want 12345", got.LastValidatedAt)
	}
}

func TestSessionSecretNeverPersisted(t *testing.T) {
	dir := t.TempDir()
	db, err := wocadb.Open("sqlite:" + filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	st, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	tenantID, userID := seedSessionTenant(t, st)

	_, full, err := st.IssueSession(ctx, tenantID, userID, "device-1", "tp", authz.RoleMember, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	secret := strings.SplitN(full, "_", 3)[2]
	var count int
	if err := db.DB.QueryRow(
		`SELECT COUNT(*) FROM native_sessions WHERE id_hash LIKE `+st.ph(0)+` OR device_id LIKE `+st.ph(1),
		"%"+secret+"%", "%"+secret+"%").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("session secret leaked into native_sessions (%d rows)", count)
	}
}

func seedSessionTenant(t *testing.T, st *Store) (tenantID, userID string) {
	t.Helper()
	ctx := context.Background()
	tn, err := st.CreateTenant(ctx, "Acme")
	if err != nil {
		t.Fatal(err)
	}
	u, err := st.CreateUser(ctx, tn.ID, "alice", "member")
	if err != nil {
		t.Fatal(err)
	}
	return tn.ID, u.ID
}

// A closed database surfaces errors on every session-store path.
func TestSessionStoreClosedDBSurfacesErrors(t *testing.T) {
	db, err := wocadb.Open("sqlite:" + filepath.Join(t.TempDir(), "closed.db"))
	if err != nil {
		t.Fatal(err)
	}
	st, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	tenantID, userID := seedSessionTenant(t, st)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	check := func(what string, err error) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s: no error on closed db", what)
		}
	}
	check("issue session", func() error {
		_, _, err := st.IssueSession(ctx, tenantID, userID, "d", "tp", authz.RoleMember, time.Hour)
		return err
	}())
	check("lookup session", func() error {
		_, _, err := st.LookupSession(ctx, "wsess_"+strings.Repeat("ab", 6)+"_"+strings.Repeat("cd", 24))
		return err
	}())
	check("revoke session", st.RevokeSession(ctx, "wsess_"+strings.Repeat("ab", 6)+"_"+strings.Repeat("cd", 24)))
	check("touch session", st.TouchSession(ctx, "wsess_"+strings.Repeat("ab", 6)+"_"+strings.Repeat("cd", 24), 1))
	check("create tenant", func() error { _, err := st.CreateTenant(ctx, "x"); return err }())
	check("create user", func() error { _, err := st.CreateUser(ctx, tenantID, "n", authz.RoleMember); return err }())
	check("issue key", func() error { _, _, err := st.IssueKey(ctx, tenantID, userID, "k", authz.RoleMember); return err }())
	check("list keys", func() error { _, err := st.ListKeys(ctx, tenantID); return err }())
	check("revoke key", st.RevokeKey(ctx, "p"))
	check("list tenants", func() error { _, err := st.ListTenants(ctx); return err }())
	check("authenticate", func() error { _, err := NewAPIKeyProvider(st).Authenticate(ctx, "woca_a_b"); return err }())
}

// sessionPrefix accepts only canonical wsess ids.
func TestSessionPrefixShapes(t *testing.T) {
	for _, bad := range []string{"", "wsess", "woca_aa_bb", "wsess_short_x", "wsess_" + strings.Repeat("g", 12) + "_x"} {
		if _, ok := sessionPrefix(bad); ok {
			t.Fatalf("sessionPrefix(%q) accepted", bad)
		}
	}
	good := "wsess_" + strings.Repeat("ab", 6) + "_" + strings.Repeat("cd", 24)
	if p, ok := sessionPrefix(good); !ok || p != strings.Repeat("ab", 6) {
		t.Fatalf("sessionPrefix(%q) = %q ok=%v", good, p, ok)
	}
}

// IssueSession rejects blank device/thumbprint before touching the store.
func TestIssueSessionInputValidation(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	tenantID, userID := seedSessionTenant(t, st)
	if _, _, err := st.IssueSession(ctx, tenantID, userID, "", "tp", authz.RoleMember, time.Hour); err == nil {
		t.Fatal("blank device accepted")
	}
	if _, _, err := st.IssueSession(ctx, tenantID, userID, "d", "", authz.RoleMember, time.Hour); err == nil {
		t.Fatal("blank thumbprint accepted")
	}
	_, _, err := st.IssueSession(ctx, tenantID, "u_missing", "d", "tp", authz.RoleMember, time.Hour)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("unknown user = %v", err)
	}
}

// ph addresses placeholders per dialect.
func TestPlaceholdersPerDialect(t *testing.T) {
	s := &Store{dialect: wocadb.DialectPostgres}
	if got := s.ph(0); got != "$1" || s.ph(2) != "$3" {
		t.Fatalf("postgres placeholders = %q %q", got, s.ph(2))
	}
	l := &Store{dialect: wocadb.DialectSQLite}
	if l.ph(0) != "?" {
		t.Fatalf("sqlite placeholder = %q", l.ph(0))
	}
}

func TestNewStoreOnClosedDB(t *testing.T) {
	db, err := wocadb.Open("sqlite:" + filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(db); err == nil {
		t.Fatal("NewStore on closed db succeeded")
	}
}

func TestIsUniqueViolationMatrix(t *testing.T) {
	for _, hit := range []string{
		"UNIQUE constraint failed: runs.run_id",
		"duplicate key value violates unique constraint \"runs_pkey\"",
		"ERROR 23505",
	} {
		if !isUniqueViolation(errors.New(hit)) {
			t.Fatalf("missed %q", hit)
		}
	}
	if isUniqueViolation(nil) || isUniqueViolation(errors.New("nope")) {
		t.Fatal("false positive")
	}
}
