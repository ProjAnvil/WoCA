package server

// /api/fs/* route tests: scope gating, jail semantics over HTTP, mkdir
// status codes, and the validate endpoint (product design §4.1).

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/rs/zerolog"

	"github.com/projanvil/woca/internal/fsb"
	"github.com/projanvil/woca/internal/platform/authn"
	"github.com/projanvil/woca/internal/platform/tenancy"
)

// rejectProvider denies every credential.
type rejectProvider struct{}

func (rejectProvider) Authenticate(context.Context, string) (tenancy.Context, error) {
	return tenancy.Context{}, authn.ErrUnauthenticated
}

func newFSServer(t *testing.T) (*fsb.Service, *httptest.Server) {
	t.Helper()
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	svc := fsb.New([]string{base}, false)
	s := New(Config{
		Static: fstest.MapFS{}, Logger: zerolog.Nop(),
		WorkDir: t.TempDir(), FS: svc,
		Agent: newScriptedAgent(t, "x"),
	})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return svc, ts
}

func TestFSRoutesListAndValidate(t *testing.T) {
	_, ts := newFSServer(t)

	resp, err := http.Get(ts.URL + "/api/filesystem/list")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list roots = %d", resp.StatusCode)
	}
	var roots struct {
		Roots []string                `json:"roots"`
		Dirs  []struct{ Name string } `json:"dirs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&roots); err != nil {
		t.Fatal(err)
	}
	if len(roots.Roots) != 1 || len(roots.Dirs) != 1 {
		t.Fatalf("roots listing = %+v", roots)
	}
	// Listing the root shows its child directories only.
	resp1b, err := http.Get(ts.URL + "/api/filesystem/list?path=" + roots.Roots[0])
	if err != nil {
		t.Fatal(err)
	}
	defer resp1b.Body.Close()
	var children struct {
		Dirs []struct{ Name string } `json:"dirs"`
	}
	if err := json.NewDecoder(resp1b.Body).Decode(&children); err != nil {
		t.Fatal(err)
	}
	if len(children.Dirs) != 1 || children.Dirs[0].Name != "sub" {
		t.Fatalf("children = %+v", children.Dirs)
	}

	// Validate an existing dir.
	resp2, err := http.Get(ts.URL + "/api/filesystem/validate?path=" + roots.Roots[0])
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var v struct {
		Exists bool `json:"exists"`
		IsDir  bool `json:"isDir"`
	}
	_ = json.NewDecoder(resp2.Body).Decode(&v)
	if !v.Exists || !v.IsDir {
		t.Fatalf("validate = %+v", v)
	}

	// Out-of-jail listing is a uniform 403 (no path oracle).
	resp3, err := http.Get(ts.URL + "/api/filesystem/list?path=" + t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusForbidden {
		t.Fatalf("outside jail = %d, want 403", resp3.StatusCode)
	}
}

func TestFSRoutesMkdirStatusCodes(t *testing.T) {
	_, ts := newFSServer(t)
	resp, _ := http.Get(ts.URL + "/api/filesystem/list")
	var roots struct {
		Roots []string `json:"roots"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&roots)
	resp.Body.Close()
	base := roots.Roots[0]

	mkdir := func(parent, name string) int {
		body, _ := json.Marshal(map[string]string{"parent": parent, "name": name})
		resp, err := http.Post(ts.URL+"/api/filesystem/mkdir", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := mkdir(base, "brand-new"); code != http.StatusCreated {
		t.Fatalf("mkdir = %d, want 201", code)
	}
	if st, err := os.Stat(filepath.Join(base, "brand-new")); err != nil || !st.IsDir() {
		t.Fatalf("created dir missing: %v", err)
	}
	if code := mkdir(base, "brand-new"); code != http.StatusConflict {
		t.Fatalf("mkdir dup = %d, want 409", code)
	}
	if code := mkdir(base, "../escape"); code != http.StatusBadRequest {
		t.Fatalf("mkdir bad name = %d, want 400", code)
	}
	if code := mkdir(t.TempDir(), "x"); code != http.StatusForbidden {
		t.Fatalf("mkdir outside = %d, want 403", code)
	}
}

// The unauthenticated posture: with no Auth provider the gateway trusts
// the local machine, so fs routes answer (the personal loopback default).
// With a strict provider they 401 — R2 leaves no anonymous /api surface.
func TestFSRoutesRequireAuthentication(t *testing.T) {
	base := t.TempDir()
	s := New(Config{
		Static: fstest.MapFS{}, Logger: zerolog.Nop(),
		WorkDir: t.TempDir(), FS: fsb.New([]string{base}, false),
		Agent: newScriptedAgent(t, "x"),
		Auth:  rejectProvider{},
	})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/api/filesystem/list")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous fs list = %d, want 401", resp.StatusCode)
	}
}
