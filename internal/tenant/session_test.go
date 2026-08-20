package tenant

import (
	"context"
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
