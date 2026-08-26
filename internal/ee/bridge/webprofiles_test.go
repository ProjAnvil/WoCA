package bridge

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/projanvil/woca/internal/wocadb"
)

func testLocalKV(t *testing.T) *LocalKV {
	t.Helper()
	db, err := wocadb.Open("sqlite:" + filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewLocalKV(db)
}

func passThrough(scope string) func(http.HandlerFunc) http.Handler {
	return func(h http.HandlerFunc) http.Handler { return h }
}

func TestWebProfileCRUD(t *testing.T) {
	kv := testLocalKV(t)
	mux := http.NewServeMux()
	WebProfileRoutes(mux, kv, passThrough)

	// Empty list on a fresh bridge.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/profiles", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("get empty = %d", rec.Code)
	}
	var list WebProfiles
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list.Profiles) != 0 {
		t.Fatalf("empty list = %s err=%v", rec.Body.String(), err)
	}

	// Add two remotes (origin normalized, trailing path dropped).
	add := func(raw string) WebProfile {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"url": raw, "label": "lab"})
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/profiles", bytes.NewReader(body)))
		if rec.Code != http.StatusOK {
			t.Fatalf("add %q = %d: %s", raw, rec.Code, rec.Body.String())
		}
		var p WebProfile
		if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return p
	}
	a := add("https://edge.example.com/some/path")
	b := add("http://10.0.0.5:3490")
	if a.URL != "https://edge.example.com" || b.URL != "http://10.0.0.5:3490" {
		t.Fatalf("origin normalization: %+v %+v", a, b)
	}
	if a.ID == b.ID {
		t.Fatalf("ids must differ")
	}

	// Adding the same origin again is idempotent (returns the existing
	// entry, no duplicate).
	dup := add("https://edge.example.com")
	if dup.ID != a.ID {
		t.Fatalf("duplicate add created a second profile: %+v", dup)
	}

	// Invalid URLs are rejected.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/profiles",
		bytes.NewReader([]byte(`{"url":"ftp://nope"}`))))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid url accepted: %d", rec.Code)
	}

	// Use marks the last-used pointer and survives a reload from the DB.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/profiles/"+b.ID+"/use", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("use = %d", rec.Code)
	}
	if got := LoadWebProfiles(kv).Last; got != b.ID {
		t.Fatalf("last = %q want %q", got, b.ID)
	}

	// Delete removes it and clears a dangling last pointer.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/profiles/"+b.ID, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("delete = %d: %s", rec.Code, rec.Body.String())
	}
	final := LoadWebProfiles(kv)
	if len(final.Profiles) != 1 || final.Last != "" {
		t.Fatalf("after delete: %+v", final)
	}

	// Unknown ids 404.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/profiles/p_missing", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing delete = %d", rec.Code)
	}
}

func TestNormalizeRemoteOrigin(t *testing.T) {
	cases := map[string]string{
		"  https://Edge.Example.com/path  ": "https://Edge.Example.com",
		"http://127.0.0.1:3490/":            "http://127.0.0.1:3490",
		"https://x.io":                      "https://x.io",
		"":                                  "",
		"edge.example.com":                  "",
		"ftp://x":                           "",
		"https://":                          "",
	}
	for in, want := range cases {
		if got := normalizeRemoteOrigin(in); got != want {
			t.Errorf("normalize(%q) = %q want %q", in, got, want)
		}
	}
}
