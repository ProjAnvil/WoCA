package skillsvcui

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The embedded SPA serves its index.html (synced by `make ee-web`).
func TestHandlerServesIndex(t *testing.T) {
	if _, err := fs.Stat(Root(), "index.html"); err != nil {
		t.Skip("SPA assets not built (fresh clone); run `make ee-web` to enable this test")
	}
	h := Handler()
	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("SPA index = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "app") {
		t.Fatal("SPA index has no mount point")
	}
}
