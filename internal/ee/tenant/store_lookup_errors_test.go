package tenant

// Gap tests: closed-DB error surfacing, malformed-key rejection, lookup
// miss/wrong-secret/revoked branches, and the constant-time comparator.

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/projanvil/woca/internal/wocadb"
)

func openGapStore(t *testing.T) (*Store, *wocadb.DB) {
	t.Helper()
	db, err := wocadb.Open("sqlite:" + filepath.Join(t.TempDir(), "gaps.db"))
	if err != nil {
		t.Fatal(err)
	}
	st, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return st, db
}

func TestStoreClosedDBSurfacesErrors(t *testing.T) {
	st, db := openGapStore(t)
	if _, err := st.CreateTenant(t.Context(), "x"); err != nil {
		t.Fatal(err) // warm the pool before closing
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	if _, err := st.CreateTenant(ctx, "y"); err == nil {
		t.Error("CreateTenant on closed DB should fail")
	}
	if _, err := st.ListTenants(ctx); err == nil {
		t.Error("ListTenants on closed DB should fail")
	}
	if _, err := st.CreateUser(ctx, "t", "u", "member"); err == nil {
		t.Error("CreateUser on closed DB should fail")
	}
	if _, _, err := st.IssueKey(ctx, "t", "u", "n", "member"); err == nil {
		t.Error("IssueKey on closed DB should fail")
	}
	if _, _, err := st.ListKeys(ctx, "t", "", "", "", 200, 0); err == nil {
		t.Error("ListKeys on closed DB should fail")
	}
	hex48 := strings.Repeat("a", keySecretLen)
	if _, _, err := st.lookup("woca_" + strings.Repeat("b", keyPrefixLen) + "_" + hex48); err == nil {
		t.Error("lookup on closed DB should fail")
	}
}

func TestLookupRejectsMalformedKeys(t *testing.T) {
	st, _ := openGapStore(t)
	for _, bad := range []string{
		"",
		"woca",
		"woca_only_two",
		"other_prefix_abcdefabcdef_ghijklghijklghijklghijklghijkl",
		"woca_zz!zzzzzzzz_zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz", // non-hex prefix
		"woca_zzzzzzzzzzzz_zz!zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz", // non-hex secret
		"woca_short_secret",
	} {
		if _, _, err := st.lookup(bad); err != nil {
			t.Fatalf("Authenticate(%q) should parse-reject without a DB error, got %v", bad, err)
		}
	}
}

func TestLookupUnknownPrefixWrongSecretAndRevoked(t *testing.T) {
	st, _ := openGapStore(t)
	ctx := t.Context()
	tn, err := st.CreateTenant(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	u, err := st.CreateUser(ctx, tn.ID, "alice", "member")
	if err != nil {
		t.Fatal(err)
	}
	k, full, err := st.IssueKey(ctx, tn.ID, u.ID, "primary", "member")
	if err != nil {
		t.Fatal(err)
	}

	// Unknown but well-formed prefix is a plain reject.
	ghost := "woca_" + strings.Repeat("0", keyPrefixLen) + "_" + strings.Repeat("0", keySecretLen)
	if _, ok, err := st.lookup(ghost); err != nil || ok {
		t.Fatalf("unknown prefix = ok:%v err:%v, want silent reject", ok, err)
	}

	// Right prefix, wrong secret.
	wrong := "woca_" + k.Prefix + "_" + strings.Repeat("1", keySecretLen)
	if _, ok, err := st.lookup(wrong); err != nil || ok {
		t.Fatalf("wrong secret = ok:%v err:%v, want silent reject", ok, err)
	}

	// Revoked keys stop authenticating.
	if _, _, err := st.lookup(full); err != nil {
		t.Fatalf("fresh key should authenticate: %v", err)
	}
	if err := st.RevokeKey(ctx, k.Prefix); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := st.lookup(full); err != nil || ok {
		t.Fatalf("revoked key = ok:%v err:%v, want silent reject", ok, err)
	}
}

func TestCreateUserValidationAndMissingTenant(t *testing.T) {
	st, _ := openGapStore(t)
	ctx := t.Context()
	if _, err := st.CreateUser(ctx, "t", "u", "warlock"); err == nil ||
		!strings.Contains(err.Error(), "unknown role") {
		t.Fatalf("unknown role = %v", err)
	}
	if _, err := st.CreateUser(ctx, "t_missing", "u", "member"); err == nil ||
		!strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing tenant = %v", err)
	}
}

func TestConstantTimeEqualHexMismatch(t *testing.T) {
	if constantTimeEqualHex("aa", "aabb") {
		t.Fatal("length mismatch should differ")
	}
	if !constantTimeEqualHex("deadbeef", "deadbeef") {
		t.Fatal("equal digests should match")
	}
}

func TestHashKeyStable(t *testing.T) {
	a, b := hashKey("woca_x_y"), hashKey("woca_x_y")
	if a != b || len(a) != 64 {
		t.Fatalf("hash not stable: %q vs %q", a, b)
	}
	if hashKey("woca_x_z") == a {
		t.Fatal("different inputs collided")
	}
}
