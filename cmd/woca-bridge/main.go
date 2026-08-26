// woca-bridge is the private-cloud bridge service (product design v1.7):
// the execution plane users hold themselves. It is the enterprise-edition
// server role for user machines — the same serve path as woca-server,
// with the product identity model layered on top: organization
// credentials (issued by the tenant ops console on the private core,
// verified offline against the config-delivered org key), cookie/Bearer
// login sessions with sliding expiry, and the fail-closed bind check
// (R1). The bridge carries NO key management; an unenrolled bridge keeps
// only loopback trust.
//
// Cloud access (design §6.5, v1.5): the channel to the edge access point
// — enrollment, managed-config pulls, heartbeats, and the scenario-four
// tunnel that lets any-place web command this bridge through
// https://edge.example.com — is bridge-local configuration, editable on
// the settings page (云接入) or preset with WOCA_EDGE_URL /
// WOCA_ENROLL_TOKEN.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	"github.com/projanvil/woca/internal/agentbuild"
	"github.com/projanvil/woca/internal/ee/bridge"
	"github.com/projanvil/woca/internal/ee/obs"
	"github.com/projanvil/woca/internal/gateway"
	"github.com/projanvil/woca/internal/platform/authn"
	"github.com/projanvil/woca/internal/platform/authz"
	"github.com/projanvil/woca/internal/servefront"
	"github.com/projanvil/woca/internal/server"
	"github.com/projanvil/woca/internal/settings"
	"github.com/projanvil/woca/internal/wocadb"
	webdist "github.com/projanvil/woca/web"
	"github.com/rs/zerolog"
	"github.com/spf13/cobra"
)

func main() {
	opts := &agentbuild.Options{}
	root := &cobra.Command{
		Use:           "woca-bridge",
		Short:         "woca bridge (private-cloud execution plane)",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	agentbuild.RegisterFlags(root.PersistentFlags(), opts)
	if f := root.PersistentFlags().Lookup("log-level"); f != nil {
		f.DefValue = "info"
		_ = f.Value.Set("info")
	}

	var currentAuth atomic.Pointer[bridge.Auth]
	var currentCA atomic.Pointer[bridge.CloudAccess]
	var currentStore atomic.Pointer[settings.Store]
	var serverHandler atomic.Pointer[http.Handler]
	var authMode atomic.Value // string

	ed := servefront.Edition{
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
			a := currentAuth.Load()
			if a == nil || credential == "" {
				return nil
			}
			_, err := a.Authenticate(context.Background(), credential)
			return err
		},
		Obs: func(ctx context.Context) (servefront.Observability, error) {
			return obs.Setup(ctx, obs.FromEnv())
		},
		Authn: func(st *settings.Store) (authn.Provider, server.NativeAuth, error) {
			if st == nil {
				return nil, nil, fmt.Errorf("bridge auth requires a settings database (set --db; it cannot be \"none\")")
			}
			a, err := bridge.NewAuth(st.DB())
			if err != nil {
				return nil, nil, err
			}
			currentAuth.Store(a)
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
			ca := bridge.NewCloudAccess(st, log)
			currentCA.Store(ca)
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
			currentStore.Store(st)
			ca := currentCA.Load()
			if ca == nil {
				ca = bridge.NewCloudAccess(st, zerolog.Nop())
				currentCA.Store(ca)
			}
			ca.Handler = func() http.Handler {
				if h := serverHandler.Load(); h != nil {
					return *h
				}
				return nil
			}
			ca.TunnelGate = func() bool {
				mode, _ := authMode.Load().(string)
				return mode != "" && mode != "local"
			}
			ca.Audit = func(action, detail string) {
				if a := currentAuth.Load(); a != nil {
					a.AuditEvent(context.Background(), "system", action, detail)
				}
			}
			if a := currentAuth.Load(); a != nil {
				// Channel-two identity forwarding (v1.8 §2.7): the freshly
				// built gateway client becomes Auth's introspector —
				// logins and session validations travel the machine
				// channel; the bridge holds zero user credentials.
				ca.OnChannel = a.SetIntrospector
			}
			currentCA.Store(ca)
		},
		// Model catalog (§16 pull model): in the private-cloud form the
		// authoritative catalog lives on the config plane — the bridge
		// pulls the list for its selection surface and asks the authority
		// to verify every applied model (unknown models are refused
		// before any local state is touched). Without a channel the local
		// database answers, keeping the standalone form unchanged.
		ModelCatalog: func(ctx context.Context) ([]wocadb.ModelPreset, error) {
			ca := currentCA.Load()
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
			ca := currentCA.Load()
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
			if a := currentAuth.Load(); a != nil {
				a.AuditEvent(context.Background(), actor, action, detail)
			}
		},
		Handler: func(h http.Handler) { serverHandler.Store(&h) },
		Routes: func(mux *http.ServeMux, provider authn.Provider) {
			if a, ok := provider.(*bridge.Auth); ok {
				a.RegisterRoutes(mux)
			}
			// Route gating follows the auth posture: apikey bridges check
			// scopes (cloud access is admin-only); local-trust bridges
			// have no credentials to check — the machine owner IS the
			// admin (§6.6: embedded bridges are configured from the
			// wails app).
			mode, _ := authMode.Load().(string)
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
			if ca := currentCA.Load(); ca != nil {
				bridge.CloudAccessRoutes(mux, ca, scoped)
			}
			// The managed-inventory view (远端配置): read-only,
			// settings:read — the environment-info class every member
			// reads (the claudeenv page's scope).
			bridge.ManagedInventoryRoutes(mux, scoped(authz.ScopeSettingsRead))
			// Remote profiles for the embedded web (bridge 形态的接入点
			// 切换): saved remote origins in the bridge's settings DB.
			if st := currentStore.Load(); st != nil {
				bridge.WebProfileRoutes(mux, bridge.NewLocalKV(st.DB()), scoped)
			}
			mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
				payload := map[string]any{
					"edition": "bridge",
					"auth":    tunnelAuthMode(mode),
				}
				if st := currentStore.Load(); st != nil {
					payload["managed"] = len(st.ManagedFields()) > 0
				} else {
					payload["managed"] = false
				}
				if ca := currentCA.Load(); ca != nil {
					payload["cloud"] = ca.Snapshot()
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(payload)
			})
		},
		Start: func(ctx context.Context, st *settings.Store, log zerolog.Logger) {
			if ca := currentCA.Load(); ca != nil {
				ca.WithLogger(log)
				go ca.Run(ctx)
			}
		},
	}

	serve := servefront.NewServeCmdWithEdition(opts, ed)
	// Fail-closed red line R1: refuse non-loopback binds without
	// authentication before anything starts.
	origRun := serve.RunE
	serve.RunE = func(cmd *cobra.Command, args []string) error {
		addr, _ := cmd.Flags().GetString("addr")
		authMode.Store("local")
		if mode, _ := cmd.Flags().GetString("auth"); mode != "" {
			authMode.Store(mode)
		} else if mode := os.Getenv("WOCA_AUTH"); mode != "" {
			authMode.Store(mode)
		}
		if err := bridge.CheckBind(addr, tunnelAuthMode(authMode.Load().(string))); err != nil {
			return err
		}
		return origRun(cmd, args)
	}
	root.AddCommand(serve)
	root.AddCommand(newGatewayCmd())
	root.AddCommand(servefront.NewWebCmd())
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// tunnelAuthMode normalizes the stored mode (the tunnel gate matches on
// the "not local" posture).
func tunnelAuthMode(mode string) string {
	switch mode {
	case "apikey", "api-key", "api-keys", "keys":
		return "apikey"
	}
	return mode
}
