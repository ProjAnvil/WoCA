package discovery

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestPlainValuesPassThrough(t *testing.T) {
	for _, v := range []string{
		"",
		"postgres://woca:pw@pg-primary:5432/woca?sslmode=disable",
		"sqlite:/home/u/.woca/woca.db",
		"/plain/path",
	} {
		got, err := Resolve(t.Context(), v)
		if err != nil {
			t.Fatalf("Resolve(%q): %v", v, err)
		}
		if got != strings.TrimSpace(v) {
			t.Errorf("Resolve(%q) = %q, want unchanged", v, got)
		}
	}
}

func TestNacosResolution(t *testing.T) {
	var gotGroup, gotDataID, gotTenant string
	var authorized atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/nacos/v1/auth/login":
			_ = json.NewEncoder(w).Encode(map[string]string{"accessToken": "tok-1"})
		case "/nacos/v1/cs/configs":
			if r.URL.Query().Get("accessToken") == "" {
				// Auth-enabled registry: anonymous reads are rejected.
				http.Error(w, "unknown user", http.StatusForbidden)
				return
			}
			authorized.Store(true)
			gotGroup = r.URL.Query().Get("group")
			gotDataID = r.URL.Query().Get("dataId")
			gotTenant = r.URL.Query().Get("tenant")
			_, _ = w.Write([]byte("postgres://woca:pw@pg-primary:5432/woca?sslmode=disable\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	t.Setenv("WOCA_DISCOVERY_USERNAME", "nacos")
	t.Setenv("WOCA_DISCOVERY_PASSWORD", "nacos")
	ref := "nacos://" + srv.Listener.Addr().String() + "/DEFAULT_GROUP/woca-db.dsn?namespace=prod"
	got, err := Resolve(t.Context(), ref)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "postgres://woca:pw@pg-primary:5432/woca?sslmode=disable" {
		t.Errorf("resolved = %q", got)
	}
	if !authorized.Load() {
		t.Fatal("read went through without the login token (403 retry missing)")
	}
	if gotGroup != "DEFAULT_GROUP" || gotDataID != "woca-db.dsn" || gotTenant != "prod" {
		t.Errorf("query = group %q dataId %q tenant %q", gotGroup, gotDataID, gotTenant)
	}
}

func TestNacosNoCredentialsSkipsLogin(t *testing.T) {
	loginHit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/nacos/v1/auth/login" {
			loginHit = true
		}
		_, _ = w.Write([]byte("sqlite:/data/woca.db"))
	}))
	defer srv.Close()
	t.Setenv("WOCA_DISCOVERY_USERNAME", "")

	got, err := Resolve(t.Context(), "nacos://"+srv.Listener.Addr().String()+"/g/d")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "sqlite:/data/woca.db" {
		t.Errorf("resolved = %q", got)
	}
	if loginHit {
		t.Error("login endpoint hit without credentials")
	}
}

func TestNacosErrors(t *testing.T) {
	cases := []struct {
		name string
		ref  string
		want string
	}{
		{"missing dataId", "nacos://host:8848/only-group", "nacos://<addr>/<group>/<dataId>"},
		{"http error", "", "HTTP 404"}, // wired below
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "config not found", http.StatusNotFound)
	}))
	defer srv.Close()
	cases[1].ref = "nacos://" + srv.Listener.Addr().String() + "/g/d"

	for _, c := range cases {
		_, err := Resolve(t.Context(), c.ref)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want containing %q", c.name, err, c.want)
		}
	}
}

func TestConsulResolution(t *testing.T) {
	var gotKey string
	var gotToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = strings.Trim(r.URL.Path, "/")
		gotToken = r.Header.Get("X-Consul-Token")
		enc := base64.StdEncoding.EncodeToString([]byte("postgres://resolved:5432/woca"))
		_, _ = w.Write([]byte(`[{"LockIndex":0,"Key":"` + gotKey + `","Flags":0,"Value":"` + enc + `"}]`))
	}))
	defer srv.Close()
	t.Setenv("WOCA_DISCOVERY_TOKEN", "consul-token")

	got, err := Resolve(t.Context(), "consul://"+srv.Listener.Addr().String()+"/config/woca/db")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "postgres://resolved:5432/woca" {
		t.Errorf("resolved = %q", got)
	}
	if !strings.HasSuffix(gotKey, "config/woca/db") || gotToken != "consul-token" {
		t.Errorf("request = key %q token %q", gotKey, gotToken)
	}
}

func TestConsulMissingKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()
	if _, err := Resolve(t.Context(), "consul://"+srv.Listener.Addr().String()+"/missing"); err == nil ||
		!strings.Contains(err.Error(), "not found") {
		t.Errorf("err = %v, want missing-key error", err)
	}
	if _, err := Resolve(t.Context(), "consul://"+srv.Listener.Addr().String()+"/"); err == nil {
		t.Error("empty key should error")
	}
}
