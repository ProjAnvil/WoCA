// Package gateway hosts the L1 edge middlewares shared by every deployment
// form: authentication (authn provider → tenancy context injection) and
// authorization (route scope enforcement). The personal edition injects the
// local provider, which trusts the machine; enterprise deployments inject API-key
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
// credentials. It is the only route name this package knows — every other
// exemption (bootstrap exchanges such as login/activation) is declared by
// the deployment form that mounts those routes, so core never learns an
// edition's surface vocabulary (design doc §11.3 red line 1; the seam
// discipline check-editions enforces for imports, this keeps for knowledge).
const healthPath = "/api/health"

// SessionAuthenticator authenticates a request whose Authorization header
// uses the DPoP scheme (RFC 6750 extension, RFC 9449): the access token
// alone is not sufficient — the DPoP proof header must bind it to this
// request. Implemented by tenant.SessionProvider.
type SessionAuthenticator interface {
	AuthenticateRequest(r *http.Request, accessToken string) (tenancy.Context, error)
}

// CookieCredential is optionally implemented by authn providers that also
// accept a cookie-carried session credential — browser flows need it
// because EventSource cannot set Authorization headers (R3). The cookie
// NAME is the provider's own protocol detail; core only knows that a
// provider declaring it wants the fallback.
type CookieCredential interface {
	CookieName() string
}

// Authenticate returns middleware that authenticates every /api request
// (except the health probe and the anonymousAPI bootstrap exchanges) and
// injects the resulting tenancy context into the request context. Non-API
// paths (the embedded SPA and its assets) pass through untouched: they
// carry no tenant data.
//
// anonymousAPI lists additional exact /api paths the deployment form
// mounts as bootstrap exchanges (login, activation): like OAuth token
// endpoints they cannot themselves require a credential. The form that
// mounts the route declares it here — core never hardcodes a surface.
//
// Requests with a missing, malformed, or rejected credential get 401. A
// credential is read from the Authorization header using the Bearer scheme
// (RFC 6750, matched case-insensitively), or the DPoP scheme when a session
// authenticator is installed. Any other scheme is rejected rather than
// falling back to anonymous, so misconfigured clients fail loudly.
func Authenticate(p authn.Provider, session SessionAuthenticator, anonymousAPI []string) func(http.Handler) http.Handler {
	anonymous := make(map[string]bool, len(anonymousAPI)+1)
	anonymous[healthPath] = true
	for _, path := range anonymousAPI {
		anonymous[path] = true
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isAPIPath(r.URL.Path) && !anonymous[r.URL.Path] {
				credential := ""
				if h := r.Header.Get("Authorization"); h != "" {
					switch {
					case len(h) >= 7 && strings.EqualFold(h[:7], "bearer "):
						credential = strings.TrimSpace(h[7:])
					case len(h) >= 5 && strings.EqualFold(h[:5], "dpop "):
						if session == nil {
							writeError(w, http.StatusUnauthorized, "unauthorized: unsupported authorization scheme")
							return
						}
						token := strings.TrimSpace(h[5:])
						tc, err := session.AuthenticateRequest(r, token)
						if err != nil {
							w.Header().Set("WWW-Authenticate", "DPoP")
							writeError(w, http.StatusUnauthorized, "unauthorized")
							return
						}
						r = r.WithContext(tenancy.NewContext(r.Context(), tc))
						next.ServeHTTP(w, r)
						return
					default:
						// Present but not a supported credential scheme:
						// reject rather than fall back to anonymous, so
						// misconfigured clients fail loudly.
						writeError(w, http.StatusUnauthorized, "unauthorized: unsupported authorization scheme")
						return
					}
				}
				var tc tenancy.Context
				var err error
				if session != nil && credential == "" && r.Header.Get("DPoP") != "" {
					// A DPoP proof without an Authorization token cannot
					// authenticate.
					writeError(w, http.StatusUnauthorized, "unauthorized")
					return
				}
				if credential == "" {
					// Cookie sessions (browser login): EventSource cannot
					// set Authorization headers. Only providers that
					// declare their cookie name get the fallback — the
					// name is the provider's protocol, not core's.
					if cc, ok := p.(CookieCredential); ok {
						if c, err := r.Cookie(cc.CookieName()); err == nil && c.Value != "" {
							credential = c.Value
						}
					}
				}
				tc, err = p.Authenticate(r.Context(), credential)
				if err != nil {
					w.Header().Set("WWW-Authenticate", "Bearer")
					writeCoded(w, http.StatusUnauthorized, "unauthorized", authn.CodeOf(err))
					return
				}
				r = r.WithContext(tenancy.NewContext(r.Context(), tc))
			}
			next.ServeHTTP(w, r)
		})
	}
}

func isAPIPath(p string) bool { return strings.HasPrefix(p, "/api/") }

// ExtractCredential pulls the request's bearer credential the way the
// Authenticate middleware would (Authorization header, else the declared
// session cookie). Stream revalidators use it to periodically re-check
// credentials that were only validated at connect time.
func ExtractCredential(p authn.Provider, r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		if len(h) >= 7 && strings.EqualFold(h[:7], "bearer ") {
			return strings.TrimSpace(h[7:])
		}
		if len(h) >= 5 && strings.EqualFold(h[:5], "dpop ") {
			return strings.TrimSpace(h[5:])
		}
		return ""
	}
	if cc, ok := p.(CookieCredential); ok {
		if c, err := r.Cookie(cc.CookieName()); err == nil {
			return c.Value
		}
	}
	return ""
}

// RequireScope returns middleware that enforces scope via a before the
// handler runs. It must be installed after Authenticate; requests without an
// injected tenancy context are denied (default deny, design doc §11.3).
func RequireScope(a authz.Authorizer, scope string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tc, ok := tenancy.FromContext(r.Context())
			if !ok {
				writeForbidden(w, "forbidden")
				return
			}
			if err := a.Authorize(r.Context(), tc, scope); err != nil {
				writeForbidden(w, "forbidden")
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
//
// exempt lists path prefixes a deployment form opens beyond the local
// tenant: bootstrap exchanges it declared anonymous (login/activation —
// the surfaces that mount them know they carry no host data) and surfaces
// that no longer touch the host, such as the queue-dispatch form's
// "/api/runs" (runs execute on workers, in a sandbox; the gateway only
// enqueues and streams ledger events).
func RequireLocalTenant(exempt ...string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == healthPath || !isAPIPath(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			for _, p := range exempt {
				if strings.HasPrefix(r.URL.Path, p) {
					next.ServeHTTP(w, r)
					return
				}
			}
			tc, ok := tenancy.FromContext(r.Context())
			if !ok || tc.TenantID != "local" {
				writeForbidden(w, "forbidden: this surface is not tenant-scoped yet")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// writeError emits the API's uniform JSON error envelope.
func writeError(w http.ResponseWriter, code int, msg string) {
	writeCoded(w, code, msg, "")
}

// writeForbidden is the 403 envelope: the enumerated forbidden code, so
// clients distinguish "who you are failed" (401 unauthorized) from "what you
// may do failed" (403 forbidden) without parsing message strings.
func writeForbidden(w http.ResponseWriter, msg string) {
	writeCoded(w, http.StatusForbidden, msg, authn.CodeForbidden)
}

// writeCoded emits the error envelope with the enumerated failure code —
// the field every client switches on ({"error": ..., "code": "revoked"}).
func writeCoded(w http.ResponseWriter, code int, msg, errCode string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	body := map[string]string{"error": msg, "code": authn.CodeUnauthorized}
	if errCode != "" {
		body["code"] = errCode
	}
	_ = json.NewEncoder(w).Encode(body)
}
