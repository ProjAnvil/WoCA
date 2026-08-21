package wocadb

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// TestOpenViaNacos verifies the §12.1 VM-deployment path: a registry
// reference in the DB target resolves before the dialect is chosen.
func TestOpenViaNacos(t *testing.T) {
	home := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("sqlite:" + filepath.ToSlash(filepath.Join(home, "resolved.db"))))
	}))
	defer srv.Close()

	db, err := Open("nacos://" + srv.Listener.Addr().String() + "/g/woca-db")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	if db.Dialect != DialectSQLite || !strings.HasSuffix(db.Path, "resolved.db") {
		t.Errorf("opened %+v %+v", db.Dialect, db.Path)
	}
}

// TestOpenViaRegistryError keeps the error path observable and redacted.
func TestOpenViaRegistryError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no config", http.StatusNotFound)
	}))
	defer srv.Close()
	addr := srv.Listener.Addr().String()
	_, err := Open("consul://" + addr + "/missing/key")
	if err == nil {
		t.Fatal("expected resolution error")
	}
	if strings.Contains(err.Error(), addr) {
		t.Errorf("error leaks the registry host: %v", err)
	}
}
