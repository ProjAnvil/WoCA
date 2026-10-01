// Serve-front edition assembly for the bridge execution plane: the exact
// wiring cmd/woca-bridge attaches to the shared serve path, extracted so
// test harnesses (e2e/lab) assemble the identical bridge instead of a
// drifting copy. The command layer keeps flag parsing, the bind red line,
// and the gateway subcommand; everything edition-shaped lives here.
package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"

	"github.com/projanvil/woca/internal/ee/obs"
	"github.com/projanvil/woca/internal/gateway"
	"github.com/projanvil/woca/internal/platform/authn"
	"github.com/projanvil/woca/internal/platform/authz"
	"github.com/projanvil/woca/internal/servefront"
	"github.com/projanvil/woca/internal/server"
	"github.com/projanvil/woca/internal/settings"
	"github.com/projanvil/woca/internal/wocadb"
	webdist "github.com/projanvil/woca/web"
)

// ServeState carries the live cross-section the serve-front edition reads:
// the freshly built auth provider, the cloud-access channel, the settings
// store, the fully-built server handler (the tunnel's execution target),
// and the auth posture flag parsing decided on. cmd/woca-bridge owns one;
// test harnesses own their own instead of re-deriving the wiring.
type ServeState struct {
	Auth     atomic.Pointer[Auth]
	CA       atomic.Pointer[CloudAccess]
	Store    atomic.Pointer[settings.Store]
	Handler  atomic.Pointer[http.Handler]
	AuthMode atomic.Value // string; empty means unset → local trust
}

// Mode returns the effective auth posture ("local" until the command says
// otherwise), matching cmd/woca-bridge's RunE defaulting.
func (s *ServeState) Mode() string {
	if m, ok := s.AuthMode.Load().(string); ok && m != "" {
		return m
	}
	return "local"
}

// TunnelMode is the normalized posture the tunnel gate and the status
// payload match on ("apikey" for every key-spelled variant).
func (s *ServeState) TunnelMode() string { return TunnelAuthMode(s.Mode()) }

// Edition builds the servefront assembly for the bridge execution plane.
// Callers must store the auth posture on the state before serving starts.
func (s *ServeState) Edition() servefront.Edition {
	return servefront.Edition{
		// The bridge ships the full chat UI: it is the execution plane
		// users command over the web (scenario four tunnels its serve
		// surface through the edge).
		Static: webdist.DistFS(),
		// The bridge's bootstrap exchange (R2): the login route this
		// entry mounts via Auth.RegisterRoutes is anonymous and carries
		// no host data past the gate.
		AnonymousAPIPaths:      []string{"/api/auth/login"},
		HostGateExemptPrefixes: []string{"/api/auth/login"},
		// Revocation closing: open streams revalidate their credential
		// against the auth provider, so a revoked key/user breaks active
		// SSE with an enumerated auth_revoked event.
		StreamReauth: func(credential string) error {
			a := s.Auth.Load()
			if a == nil || credential == "" {
				return nil
			}
			_, err := a.Authenticate(context.Background(), credential)
			return err
		},
		Obs: func(ctx context.Context) (servefront.Observability, error) {
			cfg := obs.FromEnv()
			// The /gw/obs telemetry push authenticates with the enrolled
			// machine identity: the exporter re-reads the live token on
			// every export (rotation-safe; empty while unenrolled, which
			// the edge refuses by design).
			cfg.OTLPTokenProvider = func() string {
				if ca := s.CA.Load(); ca != nil {
					return ca.MachineToken()
				}
				return ""
			}
			o, err := obs.Setup(ctx, cfg)
			if err != nil {
				return nil, err
			}
			// The bridge opts out of the per-request HTTP span plane
			// (its business surface is the local chat UI; healthz/metrics
			// via Wrap stay). Embedding only the seam interface hides the
			// concrete method servefront's optional assertion looks for.
			return spansOff{Observability: o}, nil
		},
		Authn: func(st *settings.Store) (authn.Provider, server.NativeAuth, error) {
			if st == nil {
				return nil, nil, fmt.Errorf("bridge auth requires a settings database (set --db; it cannot be \"none\")")
			}
			a, err := NewAuth(st.DB())
			if err != nil {
				return nil, nil, err
			}
			s.Auth.Store(a)
			return a, nil, nil
		},
		// Pre-build config pull (product §3 storage domain): one blocking
		// pull installs the managed overlay before the agent build reads
		// settings, so the config-delivered postgres/memory coordinates
		// are effective when the build-time connections open. The steady
		// state stays with the Run loop. The CloudAccess instance is kept
		// for OnSettings to wire callbacks onto.
		PreBuildConfig: func(ctx context.Context, st *settings.Store, log zerolog.Logger) {
			if st == nil {
				return
			}
			ca := NewCloudAccess(st, log)
			s.CA.Store(ca)
			pullCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			ca.PullNow(pullCtx)
		},
		// Cloud access (design §6.5/§3.3) rides the settings database and
		// is NOT auth-dependent: a local-trust bridge (the embedded
		// default) still enrolls and pulls managed config, and its owner
		// configures it from the wails app (§6.6 入口① 的配置落点).
		OnSettings: func(st *settings.Store) {
			if st == nil {
				return
			}
			s.Store.Store(st)
			ca := s.CA.Load()
			if ca == nil {
				ca = NewCloudAccess(st, zerolog.Nop())
				s.CA.Store(ca)
			}
			ca.Handler = func() http.Handler {
				if h := s.Handler.Load(); h != nil {
					return *h
				}
				return nil
			}
			ca.TunnelGate = func() bool { return s.TunnelMode() != "local" }
			ca.Audit = func(action, detail string) {
				if a := s.Auth.Load(); a != nil {
					a.AuditEvent(context.Background(), "system", action, detail)
				}
			}
			if a := s.Auth.Load(); a != nil {
				// Channel-two identity forwarding (v1.8 §2.7): the freshly
				// built gateway client becomes Auth's introspector —
				// logins and session validations travel the machine
				// channel; the bridge holds zero user credentials.
				ca.OnChannel = a.SetIntrospector
			}
			s.CA.Store(ca)
		},
		// Model catalog (§16 pull model): in the private-cloud form the
		// authoritative catalog lives on the config plane — the bridge
		// pulls the list for its selection surface and asks the authority
		// to verify every applied model (unknown models are refused
		// before any local state is touched). Without a channel the local
		// database answers, keeping the standalone form unchanged.
		ModelCatalog: func(ctx context.Context) ([]wocadb.ModelPreset, error) {
			ca := s.CA.Load()
			if ca == nil {
				return nil, nil
			}
			fetched, err := ca.FetchModels(ctx)
			if err != nil || fetched == nil {
				return nil, err
			}
			out := make([]wocadb.ModelPreset, 0, len(fetched))
			for _, f := range fetched {
				out = append(out, wocadb.ModelPreset{
					ID: f.ID, Name: f.Name, Provider: f.Provider, Model: f.Model,
					MaxTokens: f.MaxTokens, Temperature: f.Temperature, BaseURL: f.BaseURL,
				})
			}
			return out, nil
		},
		ResolveModel: func(ctx context.Context, id, provider, model string) (wocadb.ModelPreset, string, bool) {
			ca := s.CA.Load()
			if ca == nil {
				return wocadb.ModelPreset{}, "", false
			}
			f, err := ca.VerifyModel(ctx, id, provider, model)
			if err != nil {
				return wocadb.ModelPreset{}, "", false
			}
			return wocadb.ModelPreset{
				ID: f.ID, Name: f.Name, Provider: f.Provider, Model: f.Model,
				MaxTokens: f.MaxTokens, Temperature: f.Temperature, BaseURL: f.BaseURL,
			}, f.APIKey, true
		},
		Audit: func(actor, action, detail string) {
			// The actor arrives as a user id; Auth.audit resolves the
			// key remark (the person) before recording.
			if a := s.Auth.Load(); a != nil {
				a.AuditEvent(context.Background(), actor, action, detail)
			}
		},
		Handler: func(h http.Handler) { s.Handler.Store(&h) },
		Routes: func(mux *http.ServeMux, provider authn.Provider) {
			if a, ok := provider.(*Auth); ok {
				a.RegisterRoutes(mux)
			}
			// Route gating follows the auth posture: apikey bridges check
			// scopes (cloud access is admin-only); local-trust bridges
			// have no credentials to check — the machine owner IS the
			// admin (§6.6: embedded bridges are configured from the
			// wails app).
			mode := s.Mode()
			var scoped func(scope string) func(http.HandlerFunc) http.Handler
			if mode != "" && mode != "local" {
				scoped = func(scope string) func(http.HandlerFunc) http.Handler {
					return func(h http.HandlerFunc) http.Handler {
						return gateway.RequireScope(authz.NewLocal(), scope)(h)
					}
				}
			} else {
				scoped = func(scope string) func(http.HandlerFunc) http.Handler {
					return func(h http.HandlerFunc) http.Handler { return h }
				}
			}
			if ca := s.CA.Load(); ca != nil {
				CloudAccessRoutes(mux, ca, scoped)
			}
			// The managed-inventory view (远端配置): read-only,
			// settings:read — the environment-info class every member
			// reads (the claudeenv page's scope).
			ManagedInventoryRoutes(mux, scoped(authz.ScopeSettingsRead))
			// Remote profiles for the embedded web (bridge 形态的接入点
			// 切换): saved remote origins in the bridge's settings DB.
			if st := s.Store.Load(); st != nil {
				WebProfileRoutes(mux, NewLocalKV(st.DB()), scoped)
			}
			mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
				payload := map[string]any{
					"edition": "bridge",
					"auth":    TunnelAuthMode(mode),
				}
				if st := s.Store.Load(); st != nil {
					payload["managed"] = len(st.ManagedFields()) > 0
				} else {
					payload["managed"] = false
				}
				if ca := s.CA.Load(); ca != nil {
					payload["cloud"] = ca.Snapshot()
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(payload)
			})
		},
		Start: func(ctx context.Context, _ servefront.Observability, st *settings.Store, log zerolog.Logger) {
			if ca := s.CA.Load(); ca != nil {
				ca.WithLogger(log)
				go ca.Run(ctx)
			}
		},
	}
}

// TunnelAuthMode normalizes a posture string for the tunnel gate and the
// status payload (the "not local" match).
func TunnelAuthMode(mode string) string {
	switch mode {
	case "apikey", "api-key", "api-keys", "keys":
		return "apikey"
	}
	return mode
}

// spansOff hides the obs HTTP span middleware from servefront's optional
// interface while keeping every Observability seam method (Install, Wrap,
// Shutdown, SetRunState) intact.
type spansOff struct {
	servefront.Observability
}
