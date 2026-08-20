package gateway

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/projanvil/woca/internal/platform/authn"
	"github.com/projanvil/woca/internal/platform/authz"
	"github.com/projanvil/woca/internal/platform/tenancy"
)

// okHandler records that the request reached the handler and carries a
// tenancy context.
type okHandler struct {
	reached bool
	tc      tenancy.Context
	ok      bool
}

func (h *okHandler) ServeHTTP(_ http.ResponseWriter, r *http.Request) {
	h.reached = true
	h.tc, h.ok = tenancy.FromContext(r.Context())
}

func TestAuthenticate(t *testing.T) {
	tenantCtx := tenancy.Context{TenantID: "t1", UserID: "u1", Scopes: []string{authz.ScopeRunsRead}}

	tests := []struct {
		name       string
		provider   authn.Provider
		header     string
		path       string
		wantStatus int // 0 means the request reaches the handler
		wantTC     tenancy.Context
	}{
		{
			name:     "local provider passes api route without header",
			provider: authn.NewLocal(),
			path:     "/api/runs",
			wantTC:   tenancy.Local(),
		},
		{
			name:     "static provider with bearer token",
			provider: authn.NewStatic("tok", tenantCtx),
			header:   "Bearer tok",
			path:     "/api/sessions",
			wantTC:   tenantCtx,
		},
		{
			name:     "bearer scheme is case-insensitive",
			provider: authn.NewStatic("tok", tenantCtx),
			header:   "bearer tok",
			path:     "/api/sessions",
			wantTC:   tenantCtx,
		},
		{
			name:       "static provider rejects bad token",
			provider:   authn.NewStatic("tok", tenantCtx),
			header:     "Bearer wrong",
			path:       "/api/sessions",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "non-bearer scheme rejected",
			provider:   authn.NewLocal(),
			header:     "Basic dXNlcjpwYXNz",
			path:       "/api/sessions",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:     "health path exempt",
			provider: authn.NewStatic("tok", tenantCtx),
			path:     "/api/health",
			wantTC:   tenancy.Context{}, // no context injected
		},
		{
			name:     "spa path passes through without context",
			provider: authn.NewStatic("tok", tenantCtx),
			path:     "/assets/app.js",
			wantTC:   tenancy.Context{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &okHandler{}
			srv := httptest.NewServer(Authenticate(tt.provider)(h))
			defer srv.Close()

			req, err := http.NewRequest(http.MethodGet, srv.URL+tt.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			if tt.header != "" {
				req.Header.Set("Authorization", tt.header)
			}
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()

			if tt.wantStatus != 0 {
				if resp.StatusCode != tt.wantStatus {
					t.Fatalf("status = %d, want %d", resp.StatusCode, tt.wantStatus)
				}
				if h.reached {
					t.Error("request reached handler despite rejection")
				}
				return
			}
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			if !h.reached {
				t.Fatal("request did not reach handler")
			}
			if !reflect.DeepEqual(h.tc, tt.wantTC) {
				t.Errorf("tenancy context = %+v, want %+v", h.tc, tt.wantTC)
			}
		})
	}
}

func TestAuthenticateWWWAuthenticateHeader(t *testing.T) {
	srv := httptest.NewServer(Authenticate(authn.NewStatic("tok", tenancy.Context{TenantID: "t"}))(http.NotFoundHandler()))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/runs", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := resp.Header.Get("WWW-Authenticate"); got != "Bearer" {
		t.Errorf("WWW-Authenticate = %q, want %q", got, "Bearer")
	}
}

func TestRequireScope(t *testing.T) {
	tests := []struct {
		name       string
		tc         *tenancy.Context
		scope      string
		wantStatus int
	}{
		{
			name:  "granted scope passes",
			tc:    &tenancy.Context{TenantID: "t", Scopes: []string{authz.ScopeRunsWrite}},
			scope: authz.ScopeRunsWrite,
		},
		{
			name:       "missing scope denied",
			tc:         &tenancy.Context{TenantID: "t", Scopes: []string{authz.ScopeRunsRead}},
			scope:      authz.ScopeRunsWrite,
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "no tenancy context denied (middleware order bug)",
			tc:         nil,
			scope:      authz.ScopeRunsRead,
			wantStatus: http.StatusForbidden,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &okHandler{}
			var handler http.Handler = h
			handler = RequireScope(authz.NewLocal(), tt.scope)(handler)

			r := httptest.NewRequest(http.MethodPost, "/api/runs", nil)
			if tt.tc != nil {
				r = r.WithContext(tenancy.NewContext(r.Context(), *tt.tc))
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)

			if tt.wantStatus != 0 {
				if w.Code != tt.wantStatus {
					t.Fatalf("status = %d, want %d", w.Code, tt.wantStatus)
				}
				if h.reached {
					t.Error("request reached handler despite denial")
				}
				return
			}
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", w.Code)
			}
		})
	}
}

// TestChainLocalProvider: the full chain in personal mode must pass every
// request — the regression guard for "same code path, relaxed policy".
func TestChainLocalProvider(t *testing.T) {
	h := &okHandler{}
	handler := http.Handler(h)
	handler = RequireScope(authz.NewLocal(), authz.ScopeSettingsWrite)(handler)
	handler = Authenticate(authn.NewLocal())(handler)

	r := httptest.NewRequest(http.MethodPut, "/api/settings", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (local mode must not block)", w.Code)
	}
	if !h.ok {
		t.Fatal("tenancy context missing after local authentication")
	}
	if !reflect.DeepEqual(h.tc, tenancy.Local()) {
		t.Errorf("tenancy context = %+v, want local", h.tc)
	}
}
