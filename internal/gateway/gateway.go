// Package gateway hosts the L1 edge middlewares shared by every deployment
// form: authentication (authn provider → tenancy context injection) and
// authorization (route scope enforcement). The personal edition injects the
// local provider, which trusts the machine; server editions inject API-key
// or OIDC providers — same code path, different policy (design doc §3.3,
// §11).
package gateway

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/projanvil/woca/internal/platform/authn"
	"github.com/projanvil/woca/internal/platform/authz"
	"github.com/projanvil/woca/internal/platform/tenancy"
)

// healthPath is exempt from authentication: it is an infrastructure liveness
// probe, not an API resource, and load balancers must reach it without
// credentials. Every other route — including /api/settings and
// /api/claudeenv — goes through the middleware chain unconditionally
// (design doc §11.3, red line 1).
const healthPath = "/api/health"

// Authenticate returns middleware that authenticates every /api request
// (except the health probe) via p and injects the resulting tenancy context
// into the request context. Non-API paths (the embedded SPA and its assets)
// pass through untouched: they carry no tenant data.
//
// Requests with a missing, malformed, or rejected credential get 401. A
// credential is read from the Authorization header using the Bearer scheme
// (RFC 6750); the scheme prefix is matched case-insensitively.
func Authenticate(p authn.Provider) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != healthPath && strings.HasPrefix(r.URL.Path, "/api/") {
				credential := ""
				if h := r.Header.Get("Authorization"); h != "" {
					if len(h) >= 7 && strings.EqualFold(h[:7], "bearer ") {
						credential = strings.TrimSpace(h[7:])
					} else {
						// Present but not a Bearer credential: reject rather
						// than fall back to anonymous, so misconfigured
						// clients fail loudly.
						writeError(w, http.StatusUnauthorized, "unauthorized: unsupported authorization scheme")
						return
					}
				}
				tc, err := p.Authenticate(r.Context(), credential)
				if err != nil {
					w.Header().Set("WWW-Authenticate", "Bearer")
					writeError(w, http.StatusUnauthorized, "unauthorized")
					return
				}
				r = r.WithContext(tenancy.NewContext(r.Context(), tc))
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireScope returns middleware that enforces scope via a before the
// handler runs. It must be installed after Authenticate; requests without an
// injected tenancy context are denied (default deny, design doc §11.3).
func RequireScope(a authz.Authorizer, scope string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tc, ok := tenancy.FromContext(r.Context())
			if !ok {
				writeError(w, http.StatusForbidden, "forbidden")
				return
			}
			if err := a.Authorize(r.Context(), tc, scope); err != nil {
				writeError(w, http.StatusForbidden, "forbidden")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireLocalTenant gates host-surface routes to the local machine tenant.
//
// Phase 1 posture (design doc §11.3 red lines 2–3): every remaining /api
// route reads or drives the host — workspaces are server-local directories,
// tool execution runs in-process with no sandbox, settings hold machine
// secrets. Until the sandboxed executor lands (Phase 3), a remote tenant
// must not reach any of it, so this middleware denies every non-local
// tenant on /api (except the health probe). It wraps the same mux for every
// deployment form; the personal edition authenticates as the local tenant
// and never trips it.
func RequireLocalTenant() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == healthPath || !strings.HasPrefix(r.URL.Path, "/api/") {
				next.ServeHTTP(w, r)
				return
			}
			tc, ok := tenancy.FromContext(r.Context())
			if !ok || tc.TenantID != "local" {
				writeError(w, http.StatusForbidden, "forbidden: this surface is not tenant-scoped yet")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// writeError emits the API's uniform JSON error envelope.
func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
