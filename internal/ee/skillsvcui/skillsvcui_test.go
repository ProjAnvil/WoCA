package skillsvcui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The embedded SPA serves its index.html (synced by `make ee-web`).
func TestHandlerServesIndex(t *testing.T) {
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
