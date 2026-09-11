package skillsvc

// The HTTP API contract the console SPA (ee/skillsvc-web) is built
// against: asset CRUD, immutable publishes (server-derived checksum,
// idempotent on identical content), and label set/resolve.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func doJSON(t *testing.T, h http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rdr)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestAPILifecycle(t *testing.T) {
	h := NewHandler(openStore(t))

	w := doJSON(t, h, "POST", "/api/skills", map[string]string{"name": "code-review", "description": "reviews code"})
	if w.Code != http.StatusOK {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	var a Asset
	if err := json.Unmarshal(w.Body.Bytes(), &a); err != nil || a.ID == "" {
		t.Fatalf("create body = %q err=%v", w.Body, err)
	}

	body := "# review checklist v1"
	w = doJSON(t, h, "POST", "/api/skills/"+a.ID+"/versions", map[string]string{"body": body})
	if w.Code != http.StatusOK {
		t.Fatalf("publish = %d %s", w.Code, w.Body)
	}
	var v Version
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(body))
	if v.Version != 1 || v.Checksum != hex.EncodeToString(sum[:]) {
		t.Fatalf("version = %+v, want v1 with sha256 checksum", v)
	}

	// Identical content republishes idempotently onto the same version.
	w = doJSON(t, h, "POST", "/api/skills/"+a.ID+"/versions", map[string]string{"body": body})
	var vAgain Version
	_ = json.Unmarshal(w.Body.Bytes(), &vAgain)
	if vAgain.Version != 1 {
		t.Fatalf("idempotent republish = v%d, want v1", vAgain.Version)
	}

	w = doJSON(t, h, "PUT", "/api/skills/"+a.ID+"/labels/production", map[string]int64{"version": 1})
	if w.Code != http.StatusOK {
		t.Fatalf("set label = %d %s", w.Code, w.Body)
	}
	w = doJSON(t, h, "GET", "/api/skills/"+a.ID+"/labels", nil)
	var labels map[string]Version
	if err := json.Unmarshal(w.Body.Bytes(), &labels); err != nil {
		t.Fatal(err)
	}
	if labels["production"].Body != body {
		t.Fatalf("production = %+v", labels["production"])
	}

	w = doJSON(t, h, "GET", "/api/skills", nil)
	var assets []Asset
	if err := json.Unmarshal(w.Body.Bytes(), &assets); err != nil || len(assets) != 1 {
		t.Fatalf("list = %s err=%v", w.Body, err)
	}

	w = doJSON(t, h, "DELETE", "/api/skills/"+a.ID, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("delete = %d", w.Code)
	}
	w = doJSON(t, h, "GET", "/api/skills/"+a.ID+"/versions", nil)
	var versions []Version
	if err := json.Unmarshal(w.Body.Bytes(), &versions); err != nil || len(versions) != 0 {
		t.Fatalf("versions after delete = %s err=%v", w.Body, err)
	}
}

func TestAPIErrors(t *testing.T) {
	h := NewHandler(openStore(t))

	if w := doJSON(t, h, "POST", "/api/skills", map[string]string{"name": "  "}); w.Code != http.StatusBadRequest {
		t.Fatalf("blank name = %d want 400", w.Code)
	}
	if w := doJSON(t, h, "POST", "/api/skills/ghost/versions", map[string]string{"body": "x"}); w.Code != http.StatusNotFound {
		t.Fatalf("publish to ghost = %d want 404", w.Code)
	}
	if w := doJSON(t, h, "PUT", "/api/skills/ghost/labels/production", map[string]int64{"version": 1}); w.Code != http.StatusNotFound {
		t.Fatalf("label on ghost = %d want 404", w.Code)
	}
	req := httptest.NewRequest("POST", "/api/skills", bytes.NewReader([]byte("{")))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad JSON = %d want 400", w.Code)
	}
}
