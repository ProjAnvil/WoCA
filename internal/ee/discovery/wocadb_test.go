package discovery

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/projanvil/woca/internal/wocadb"
)

// withResolver installs the discovery resolver for the duration of a test
// (server-role entry points do this at startup).
func withResolver(t *testing.T) {
	t.Helper()
	prev := wocadb.DSNResolver
	wocadb.DSNResolver = Resolve
	t.Cleanup(func() { wocadb.DSNResolver = prev })
}

// TestOpenViaNacos verifies the §12.1 VM-deployment path: a registry
// reference in the DB target resolves before the dialect is chosen.
func TestOpenViaNacos(t *testing.T) {
	withResolver(t)
	home := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("sqlite:" + filepath.ToSlash(filepath.Join(home, "resolved.db"))))
	}))
	defer srv.Close()

	db, err := wocadb.Open("nacos://" + srv.Listener.Addr().String() + "/g/woca-db")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	if db.Dialect != wocadb.DialectSQLite || !strings.HasSuffix(db.Path, "resolved.db") {
		t.Errorf("opened %+v %+v", db.Dialect, db.Path)
	}
}

// TestOpenViaRegistryError keeps the error path observable and redacted.
func TestOpenViaRegistryError(t *testing.T) {
	withResolver(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no config", http.StatusNotFound)
	}))
	defer srv.Close()
	addr := srv.Listener.Addr().String()
	_, err := wocadb.Open("consul://" + addr + "/missing/key")
	if err == nil {
		t.Fatal("expected resolution error")
	}
	if strings.Contains(err.Error(), addr) {
		t.Errorf("error leaks the registry host: %v", err)
	}
}

// TestOpenWithoutResolver pins the personal-edition posture: with no
// resolver installed, registry references fail with a clear error instead
// of silently passing through.
func TestOpenWithoutResolver(t *testing.T) {
	if wocadb.DSNResolver != nil {
		t.Skip("resolver installed by a parallel test")
	}
	_, err := wocadb.Open("nacos://registry.example:8848/g/woca-db")
	if err == nil || !strings.Contains(err.Error(), "discovery resolver") {
		t.Fatalf("err = %v, want missing-resolver error", err)
	}
}
