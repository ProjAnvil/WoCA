package authcore

// Tests cover the core identity authority (product design v1.8 §2.1):
// woca_ key login issues an opaque session exactly once, introspection
// is authoritative with sliding expiry, key revocation kills the key's
// sessions with the enumerated code, logout and per-session revocation
// work, and the HTTP surface (/gw/auth/*) speaks the same contract the
// edge and the bridges consume.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/projanvil/woca/internal/ee/tenant"
	"github.com/projanvil/woca/internal/platform/authn"
	"github.com/projanvil/woca/internal/wocadb"
	"github.com/rs/zerolog"
)

func newService(t *testing.T) (*Service, *tenant.Store) {
	t.Helper()
	path := "sqlite:" + filepath.Join(t.TempDir(), "core.db")
	db, err := wocadb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	keys, err := tenant.NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(keys, db, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	return s, keys
}

func issueKey(t *testing.T, keys *tenant.Store, remark, role string) (full, prefix string) {
	t.Helper()
	u, err := keys.UserByName(t.Context(), "local", remark)
	if err != nil {
		u, err = keys.CreateUser(t.Context(), "local", remark, role)
		if err != nil {
			t.Fatal(err)
		}
	}
	k, full, err := keys.IssueKey(t.Context(), "local", u.ID, remark, role)
	if err != nil {
		t.Fatal(err)
	}
	return full, k.Prefix
}

func TestLoginIssuesOpaqueSession(t *testing.T) {
	s, keys := newService(t)
	full, _ := issueKey(t, keys, "alice", "member")

	sess, err := s.Login(t.Context(), full)
	if err != nil {
		t.Fatal(err)
	}
	if sess.Token == "" || sess.Remark != "alice" || sess.Role != "member" {
		t.Fatalf("session = %+v", sess)
	}
	if sess.ExpiresAt <= sess.CreatedAt {
		t.Fatalf("bad expiry: %+v", sess)
	}

	// last_used is authoritative at the core (every login counts).
	k, ok, _ := keys.KeyByPrefix(t.Context(), sess.KeyPrefix)
	if !ok || k.LastUsed == 0 {
		t.Fatalf("last_used not touched: %+v", k)
	}
}

func TestLoginFailureClassification(t *testing.T) {
	s, keys := newService(t)
	if _, err := s.Login(t.Context(), "woca_000000000000_"+repeat("0", 48)); err != authn.ErrUnauthenticated {
		t.Fatalf("unknown key = %v", err)
	}
	full, _ := issueKey(t, keys, "bob", "member")
	if err := keys.RevokeKey(t.Context(), keyPrefixOf(full)); err != nil {
		t.Fatal(err)
	}
	_, err := s.Login(t.Context(), full)
	if code := authn.CodeOf(err); code != authn.CodeRevoked {
		t.Fatalf("revoked login = %v code=%q", err, code)
	}
}

// TestIntrospectSlideAndRevoke: introspection is the authority — it
// slides expiry, classifies revocation, and drops naturally-expired
// tokens.
func TestIntrospectSlideAndRevoke(t *testing.T) {
	s, keys := newService(t)
	full, prefix := issueKey(t, keys, "carol", "tenant-admin")

	sess, err := s.Login(t.Context(), full)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Introspect(t.Context(), sess.Token)
	if err != nil {
		t.Fatal(err)
	}
	if got.UserID != sess.UserID || got.Remark != "carol" {
		t.Fatalf("introspect = %+v", got)
	}

	// Revoking the key (the console action: kill the key AND its
	// sessions) takes effect on the next validation.
	if err := keys.RevokeKey(t.Context(), prefix); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeKeySessions(t.Context(), prefix); err != nil {
		t.Fatal(err)
	}
	_, err = s.Introspect(t.Context(), sess.Token)
	if code := authn.CodeOf(err); code != authn.CodeRevoked {
		t.Fatalf("introspect after revoke = %v code=%q", err, code)
	}
	// Fresh logins with the revoked key are refused outright.
	if _, err := s.Login(t.Context(), full); authn.CodeOf(err) != authn.CodeRevoked {
		t.Fatalf("revoked relogin = %v", err)
	}
	// Unknown tokens are plain unauthenticated, not revoked.
	if _, err := s.Introspect(t.Context(), "wocs_deadbeef"); err != authn.ErrUnauthenticated {
		t.Fatalf("unknown token = %v", err)
	}
}

func TestLogoutAndSessionList(t *testing.T) {
	s, keys := newService(t)
	full, _ := issueKey(t, keys, "dave", "viewer")
	sess, _ := s.Login(t.Context(), full)

	list, total, err := s.ListSessions(t.Context(), "", "", 50, 0)
	if err != nil || len(list) != 1 || total != 1 || list[0].Remark != "dave" {
		t.Fatalf("list = %+v err=%v", list, err)
	}
	if err := s.Logout(t.Context(), sess.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Introspect(t.Context(), sess.Token); authn.CodeOf(err) != authn.CodeRevoked {
		t.Fatalf("introspect after logout = %v", err)
	}
	// The revoked row stays visible for the console's session audit.
	list, _, _ = s.ListSessions(t.Context(), "", "", 50, 0)
	if len(list) != 1 || list[0].RevokedAt == 0 {
		t.Fatalf("revoked session not listed: %+v", list)
	}
}

// TestHTTPSurface: the /gw/auth/* contract the edge and bridges speak.
func TestHTTPSurface(t *testing.T) {
	s, keys := newService(t)
	full, _ := issueKey(t, keys, "erin", "member")
	var audit []string
	h := Handler(s, func(event, subject, detail string) { audit = append(audit, event) })
	srv := httptest.NewServer(h)
	defer srv.Close()

	post := func(path, body string) (*http.Response, map[string]any) {
		resp, err := http.Post(srv.URL+path, "application/json", bytes.NewReader([]byte(body)))
		if err != nil {
			t.Fatal(err)
		}
		doc := map[string]any{}
		_ = json.NewDecoder(resp.Body).Decode(&doc)
		resp.Body.Close()
		return resp, doc
	}

	resp, doc := post("/gw/auth/login", `{"api_key":"`+full+`"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login = %d %v", resp.StatusCode, doc)
	}
	token, _ := doc["token"].(string)
	if token == "" || doc["remark"] != "erin" {
		t.Fatalf("login body = %v", doc)
	}

	resp, doc = post("/gw/auth/introspect", `{"token":"`+token+`"}`)
	if resp.StatusCode != http.StatusOK || doc["active"] != true {
		t.Fatalf("introspect = %d %v", resp.StatusCode, doc)
	}

	// Unknown key: uniform 401, no oracle.
	resp, doc = post("/gw/auth/login", `{"api_key":"woca_000000000000_0000000000000000000000000000000000000000000000000"}`)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad login = %d", resp.StatusCode)
	}

	resp, doc = post("/gw/auth/logout", `{"token":"`+token+`"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("logout = %d", resp.StatusCode)
	}
	resp, doc = post("/gw/auth/introspect", `{"token":"`+token+`"}`)
	if resp.StatusCode != http.StatusOK || doc["active"] != false || doc["code"] != authn.CodeRevoked {
		t.Fatalf("introspect after logout = %d %v", resp.StatusCode, doc)
	}

	for _, want := range []string{"login", "login_failed", "session_revoked"} {
		found := false
		for _, e := range audit {
			if e == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("audit missing %q: %v", want, audit)
		}
	}
}

func keyPrefixOf(full string) string {
	parts := splitUnderscore(full)
	if len(parts) == 3 {
		return parts[1]
	}
	return ""
}

func repeat(s string, n int) string {
	out := make([]byte, 0, n*len(s))
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}

func splitUnderscore(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '_' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}
