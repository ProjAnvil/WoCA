// cloudaccess.go is the bridge's cloud-access configuration layer
// (product design §6.5/§3.3): the edge URL, one-time enrollment token,
// registration state, and the TOFU certificate pin — all bridge-local
// infrastructure settings, deliberately outside the config plane's
// managed vocabulary. The two layers never mix: applyManaged drops any
// edge.* key the config plane tries to push, and this layer writes only
// the enumerated edge.* keys (越界写入拒绝并记审计, §3.3).
//
// Persistence lives in the settings database: edge.* keys in the plain
// settings table, the machine private key in the encrypted secrets store.
// The legacy ~/.woca/bridge-gateway.json state migrates on first boot.
package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/rs/zerolog"

	"github.com/projanvil/woca/internal/platform/authz"
	"github.com/projanvil/woca/internal/settings"
	"github.com/projanvil/woca/internal/wocadb"
)

// Bridge-local settings keys (design §3.3 storage table).
const (
	KeyEdgeURL         = "edge.url"
	KeyEdgeToken       = "edge.enroll_token"
	KeyEdgeBridgeID    = "edge.bridge_id"
	KeyEdgeAccess      = "edge.access_token"
	KeyEdgeAccessExp   = "edge.access_expires_at"
	KeyEdgeSPKIPin     = "edge.spki_pin"
	SecretEdgeIdentity = "bridge.edge.private_key"
)

// LocalKV reads and writes the bridge-local edge.* keys on the shared
// settings table.
type LocalKV struct{ db *wocadb.DB }

// NewLocalKV builds the local-settings accessor.
func NewLocalKV(db *wocadb.DB) *LocalKV { return &LocalKV{db: db} }

func (kv *LocalKV) ph(i int) string { return kv.db.Dialect.Placeholder(i) }

// Get returns one local setting ("" when unset).
func (kv *LocalKV) Get(key string) string {
	var v string
	_ = kv.db.QueryRow(`SELECT value FROM settings WHERE key = `+kv.ph(0), key).Scan(&v)
	return v
}

// Set upserts one local setting.
func (kv *LocalKV) Set(key, value string) error {
	_, err := kv.db.Exec(
		`INSERT INTO settings (key, value, updated_at) VALUES (`+kv.ph(0)+`, `+kv.ph(1)+`, `+kv.ph(2)+`)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value, wocadb.Now())
	return err
}

// Delete removes one local setting.
func (kv *LocalKV) Delete(key string) error {
	_, err := kv.db.Exec(`DELETE FROM settings WHERE key = `+kv.ph(0), key)
	return err
}

// settingsPersist adapts the settings database to the gateway client's
// StatePersist: identity + token in the settings table, private key in
// the encrypted secrets store.
type settingsPersist struct {
	kv *LocalKV
	st *settings.Store
}

// Load reads the persisted machine identity.
func (p *settingsPersist) Load() (stateFile, error) {
	var st stateFile
	st.BridgeID = p.kv.Get(KeyEdgeBridgeID)
	if key := p.st.GetSecret(SecretEdgeIdentity); key != "" {
		st.PrivateKey = []byte(key)
		if len(st.PrivateKey) != ed25519PrivateKeySize {
			return stateFile{}, fmt.Errorf("bridge: gateway state: bad key size")
		}
	}
	st.AccessToken = p.kv.Get(KeyEdgeAccess)
	if v := p.kv.Get(KeyEdgeAccessExp); v != "" {
		_, _ = fmt.Sscanf(v, "%d", &st.AccessExpiry)
	}
	return st, nil
}

// Save persists the machine identity.
func (p *settingsPersist) Save(st stateFile) error {
	if err := p.kv.Set(KeyEdgeBridgeID, st.BridgeID); err != nil {
		return err
	}
	if err := p.st.SetSecret(SecretEdgeIdentity, string(st.PrivateKey)); err != nil {
		return err
	}
	if err := p.kv.Set(KeyEdgeAccess, st.AccessToken); err != nil {
		return err
	}
	return p.kv.Set(KeyEdgeAccessExp, fmt.Sprintf("%d", st.AccessExpiry))
}

const ed25519PrivateKeySize = 64

// CloudAccessStatus is the settings-page view of the cloud channel.
type CloudAccessStatus struct {
	URL         string       `json:"url"`
	URLSource   string       `json:"url_source"`   // env | settings | ""
	TokenSource string       `json:"token_source"` // env | settings | "" (set = present)
	SpkiPin     string       `json:"spki_pin,omitempty"`
	TokenSet    bool         `json:"token_set"`
	Enrolled    bool         `json:"enrolled"`
	BridgeID    string       `json:"bridge_id,omitempty"`
	LastVer     string       `json:"last_version,omitempty"`
	LastPull    string       `json:"last_pull,omitempty"`
	Stale       bool         `json:"stale"`
	SpkiPinned  bool         `json:"spki_pinned"`
	Tunnel      TunnelStatus `json:"tunnel"`
}

// CloudAccess owns the bridge's cloud channel lifecycle: configuration
// (env > settings), the gateway client (enroll/pull/renew/heartbeat), and
// the scenario-four tunnel. Reconfigure at runtime restarts the channel.
type CloudAccess struct {
	kv  *LocalKV
	st  *settings.Store
	log zerolog.Logger

	// OnChannel attaches the freshly (re)built gateway client to a
	// consumer — the serve wiring feeds it to Auth as the channel-two
	// identity introspector (v1.8 §2.7: logins and session validations
	// forward through the machine channel).
	OnChannel func(Introspector)

	// Handler supplies the bridge's server handler for the tunnel;
	// TunnelGate reports whether the auth posture allows remote client
	// traffic (local trust + tunnel = a public shell, R1's twin).
	Handler    func() http.Handler
	TunnelGate func() bool
	Audit      func(action, detail string)
	// StatusFn builds the heartbeat payload (the serve wiring layers org
	// activity on top; the core's ops console folds it into per-user
	// "last used").
	StatusFn func() json.RawMessage

	envURL     string
	envToken   string
	insecure   bool
	legacyPath string

	mu     sync.Mutex
	root   context.Context
	cancel context.CancelFunc
	gw     *GatewayClient
	tunnel *TunnelClient
}

// NewCloudAccess builds the manager over the settings store.
func NewCloudAccess(st *settings.Store, log zerolog.Logger) *CloudAccess {
	// v1.5 naming is canonical; the v1.4 gateway spellings stay as
	// fallbacks so enrolled deployments keep working.
	url := os.Getenv("WOCA_EDGE_URL")
	if url == "" {
		url = os.Getenv("WOCA_GATEWAY_URL")
	}
	insecure := os.Getenv("WOCA_EDGE_INSECURE") == "1" || os.Getenv("WOCA_GATEWAY_INSECURE") == "1"
	return &CloudAccess{
		kv:         NewLocalKV(st.DB()),
		st:         st,
		log:        log,
		envURL:     url,
		envToken:   os.Getenv("WOCA_ENROLL_TOKEN"),
		insecure:   insecure,
		legacyPath: legacyStatePath(),
	}
}

// legacyStatePath resolves the v1.4 state file inside the effective woca
// home (WOCA_HOME first — containers repoint it — then the OS home).
func legacyStatePath() string {
	if home := os.Getenv("WOCA_HOME"); home != "" {
		return filepath.Join(home, "bridge-gateway.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".woca", "bridge-gateway.json")
}

// Run drives the channel until ctx ends (the serve Start hook).
func (c *CloudAccess) Run(ctx context.Context) {
	c.migrateLegacy()
	c.mu.Lock()
	c.root = ctx
	c.mu.Unlock()
	c.start()
	<-ctx.Done()
}

// PullNow performs one synchronous config pull over the persisted channel
// state — the pre-build hook (agentbuild OnSettingsReady) calls it so the
// managed overlay is installed BEFORE the agent build reads settings and
// opens its build-time connections (checkpoint/memoryStore postgres, the
// remote memory engine). A throwaway client over the same persisted state;
// the steady-state Run loop re-pulls (a 304) and owns everything after.
// Failures return false and are logged, never fatal: an unenrolled or
// unreachable bridge keeps building standalone.
func (c *CloudAccess) PullNow(ctx context.Context) bool {
	c.migrateLegacy()
	url, token, _, _ := c.resolve()
	if url == "" {
		return false
	}
	gw := NewGatewayClient(GatewayConfig{
		URL:             url,
		EnrollmentToken: token,
		Persist:         &settingsPersist{kv: c.kv, st: c.st},
		Insecure:        c.insecure,
		PinSource:       func() string { return c.kv.Get(KeyEdgeSPKIPin) },
	}, c.log)
	gw.Apply = func(kv map[string]string) error { return applyManaged(c.st, kv) }
	if err := gw.resume(); err != nil {
		c.log.Warn().Err(err).Msg("bridge: pre-build channel resume failed (building standalone)")
		return false
	}
	cfg, err := gw.Pull(ctx)
	if err != nil || cfg == nil {
		if err != nil {
			c.log.Warn().Err(err).Msg("bridge: pre-build config pull failed (building standalone)")
		}
		return false
	}
	gw.applyConfig(cfg)
	return true
}

// migrateLegacy imports the v1.4 file-based state once, then removes it.
// An identity already present in the settings database always wins — the
// file may be stale residue from an older container generation.
func (c *CloudAccess) migrateLegacy() {
	if c.legacyPath == "" {
		return
	}
	if existing, err := (&settingsPersist{kv: c.kv, st: c.st}).Load(); err == nil && existing.BridgeID != "" {
		_ = os.Remove(c.legacyPath)
		return
	}
	data, err := os.ReadFile(c.legacyPath)
	if err != nil {
		return
	}
	var st stateFile
	if err := json.Unmarshal(data, &st); err != nil || st.BridgeID == "" {
		return
	}
	p := &settingsPersist{kv: c.kv, st: c.st}
	if err := p.Save(st); err != nil {
		c.log.Warn().Err(err).Msg("bridge: legacy gateway state migration failed (keeping the file)")
		return
	}
	if err := os.Remove(c.legacyPath); err == nil {
		c.log.Info().Str("bridge", st.BridgeID).Msg("bridge: migrated gateway state into the settings database")
	}
}

// resolve returns the effective URL/token and where each came from (env
// wins; the page labels the sources, §6.5).
func (c *CloudAccess) resolve() (url, token, urlSource, tokenSource string) {
	url, urlSource = c.envURL, "env"
	if url == "" {
		url = strings.TrimSpace(c.kv.Get(KeyEdgeURL))
		if url != "" {
			urlSource = "settings"
		}
	}
	token, tokenSource = c.envToken, "env"
	if token == "" {
		token = c.kv.Get(KeyEdgeToken)
		if token != "" {
			tokenSource = "settings"
		}
	}
	return url, token, urlSource, tokenSource
}

// FetchModels pulls the model catalog through the live channel (nil
// channel = no catalog: the caller falls back to local state).
func (c *CloudAccess) FetchModels(ctx context.Context) ([]FetchedPreset, error) {
	c.mu.Lock()
	gw := c.gw
	c.mu.Unlock()
	if gw == nil {
		return nil, nil
	}
	return gw.FetchModels(ctx)
}

// VerifyModel authorizes a preferred model through the live channel.
func (c *CloudAccess) VerifyModel(ctx context.Context, id, provider, model string) (FetchedPreset, error) {
	c.mu.Lock()
	gw := c.gw
	c.mu.Unlock()
	if gw == nil {
		return FetchedPreset{}, ErrCoreUnreachable
	}
	return gw.VerifyModel(ctx, id, provider, model)
}

// channelIntrospector adapts the live gateway client to Auth's
// Introspector seam (the client rebuilds on reconfigure; Auth always
// sees the current one).
type channelIntrospector struct{ g *GatewayClient }

func (ci channelIntrospector) CoreLogin(ctx context.Context, apiKey string) (CoreSession, error) {
	return ci.g.AuthLogin(ctx, apiKey)
}

func (ci channelIntrospector) CoreIntrospect(ctx context.Context, token string) (CoreSession, error) {
	return ci.g.AuthIntrospect(ctx, token)
}

func (ci channelIntrospector) CoreLogout(ctx context.Context, token string) error {
	return ci.g.AuthLogout(ctx, token)
}

// WithLogger swaps in the structured logger once the serve path provides
// one (construction happens before logging is wired).
func (c *CloudAccess) WithLogger(log zerolog.Logger) { c.log = log }

// start (re)builds the channel under a context derived from the run
// context, so reconfigure/reset can tear the loops down without ending
// the serve lifetime.
func (c *CloudAccess) start() {
	c.mu.Lock()
	root := c.root
	c.mu.Unlock()
	if root == nil || root.Err() != nil {
		return
	}
	url, token, _, _ := c.resolve()
	if url == "" {
		c.log.Info().Msg("bridge: no edge configured — running standalone")
		return
	}
	onPin := func(pin string) {
		if err := c.kv.Set(KeyEdgeSPKIPin, pin); err != nil {
			c.log.Warn().Err(err).Msg("bridge: recording the edge SPKI pin failed")
		} else {
			c.log.Info().Str("spki", pin[:12]).Msg("bridge: pinned the edge certificate (TOFU)")
		}
	}
	pinSource := func() string { return c.kv.Get(KeyEdgeSPKIPin) }
	gw := NewGatewayClient(GatewayConfig{
		URL:             url,
		EnrollmentToken: token,
		Persist:         &settingsPersist{kv: c.kv, st: c.st},
		Insecure:        c.insecure,
		PinSource:       pinSource,
		OnPin:           onPin,
	}, c.log)
	gw.StatusFn = c.StatusFn
	gw.Apply = func(kv map[string]string) error {
		err := applyManaged(c.st, kv)
		if errors.Is(err, ErrLayerViolation) && c.Audit != nil {
			c.Audit("cloud_access.layer_violation", err.Error())
		}
		return err
	}
	if c.OnChannel != nil {
		c.OnChannel(channelIntrospector{g: gw})
	}
	// Consume the enrollment token once it succeeds (design: the token is
	// single-use; the page stops displaying it).
	gw.OnEnrolled = func() { _ = c.kv.Delete(KeyEdgeToken) }

	// The data plane (scenario four) only runs under an authenticated
	// posture: a local-trust bridge must not become publicly drivable.
	var tunnel *TunnelClient
	var h http.Handler
	if c.Handler != nil {
		h = c.Handler()
	}
	if h != nil && c.TunnelGate != nil && c.TunnelGate() {
		tunnel = NewTunnelClient(TunnelConfig{
			EdgeURL:   url,
			Token:     gw.CurrentToken,
			Handler:   h,
			Insecure:  c.insecure,
			PinSource: pinSource,
			OnPin:     onPin,
		}, c.log)
	} else if h != nil {
		c.log.Warn().Msg("bridge: tunnel disabled (apikey auth required — a local-trust bridge must not carry public client traffic)")
	}

	ctx, cancel := context.WithCancel(root)
	c.mu.Lock()
	c.cancel = cancel
	c.gw = gw
	c.tunnel = tunnel
	c.mu.Unlock()
	go gw.Run(ctx, c.st)
	if tunnel != nil {
		go tunnel.Run(ctx)
	}
}

// stop tears the running channel down (reconfigure/reset).
func (c *CloudAccess) stop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cancel != nil {
		c.cancel()
		c.cancel = nil
	}
	c.gw = nil
	c.tunnel = nil
}

// restart = stop + start: the old loops exit on their cancelled context
// while fresh clients resume from the persisted identity.
func (c *CloudAccess) restart() {
	c.stop()
	c.start()
}

// Snapshot returns the settings-page view.
func (c *CloudAccess) Snapshot() CloudAccessStatus {
	url, token, urlSource, tokenSource := c.resolve()
	c.mu.Lock()
	gw := c.gw
	tun := c.tunnel
	c.mu.Unlock()
	st := CloudAccessStatus{
		URL:         url,
		URLSource:   urlSource,
		TokenSource: tokenSource,
		TokenSet:    token != "",
		SpkiPin:     c.kv.Get(KeyEdgeSPKIPin),
		SpkiPinned:  c.kv.Get(KeyEdgeSPKIPin) != "",
	}
	if gw != nil {
		g := gw.Status()
		st.Enrolled = g.Enrolled
		st.BridgeID = g.BridgeID
		st.LastVer = g.LastVersion
		st.LastPull = g.LastPullISO
		st.Stale = g.Stale
	}
	if tun != nil {
		st.Tunnel = tun.Status()
	}
	return st
}

// ErrEnvPinsURL reports that the environment owns the URL (the page shows
// the source; a conflicting write is refused rather than silently
// shadowed).
var ErrEnvPinsURL = errors.New("edge.url is pinned by the WOCA_EDGE_URL environment variable")

// Configure updates the local cloud-access settings and restarts the
// channel (design §6.5: the web config page's editable surface).
func (c *CloudAccess) Configure(url, enrollmentToken string) error {
	if c.envURL != "" && url != c.envURL {
		return ErrEnvPinsURL
	}
	if c.envURL != "" {
		url = c.envURL // env wins; the rest of the write is still allowed
	}
	if err := c.kv.Set(KeyEdgeURL, strings.TrimSpace(url)); err != nil {
		return err
	}
	if enrollmentToken != "" {
		if err := c.kv.Set(KeyEdgeToken, enrollmentToken); err != nil {
			return err
		}
	}
	if c.Audit != nil {
		c.Audit("cloud_access.configure", "url="+strings.TrimSpace(url))
	}
	c.restart()
	return nil
}

// Reset deregisters the local identity (注销): the edge.* keys and the
// machine key disappear; server-side revocation stays the admin's action.
func (c *CloudAccess) Reset() error {
	for _, k := range []string{KeyEdgeURL, KeyEdgeToken, KeyEdgeBridgeID, KeyEdgeAccess, KeyEdgeAccessExp, KeyEdgeSPKIPin} {
		if err := c.kv.Delete(k); err != nil {
			return err
		}
	}
	if err := c.st.SetSecret(SecretEdgeIdentity, ""); err != nil {
		return err
	}
	if c.Audit != nil {
		c.Audit("cloud_access.reset", "local identity wiped")
	}
	c.restart()
	return nil
}

// CloudAccessRoutes mounts the settings-page API. Cloud access is the
// owner's governance surface: with members holding settings:read for the
// interactive surfaces, the whole section — reads included — sits behind
// settings:write, the admin authority.
func CloudAccessRoutes(mux *http.ServeMux, ca *CloudAccess, admin func(scope string) func(http.HandlerFunc) http.Handler) {
	mux.Handle("GET /api/cloudaccess", admin(authz.ScopeSettingsWrite)(ca.handleGet))
	mux.Handle("PUT /api/cloudaccess", admin(authz.ScopeSettingsWrite)(ca.handlePut))
	mux.Handle("POST /api/cloudaccess/reset", admin(authz.ScopeSettingsWrite)(ca.handleReset))
}

func (c *CloudAccess) handleGet(w http.ResponseWriter, _ *http.Request) {
	writeJSONBridge(w, http.StatusOK, c.Snapshot())
}

func (c *CloudAccess) handlePut(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL             string `json:"url"`
		EnrollmentToken string `json:"enrollment_token"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
		writeJSONBridge(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return
	}
	if req.URL != "" && !strings.HasPrefix(req.URL, "http://") && !strings.HasPrefix(req.URL, "https://") {
		writeJSONBridge(w, http.StatusBadRequest, map[string]string{"error": "url must start with http:// or https://"})
		return
	}
	if err := c.Configure(req.URL, req.EnrollmentToken); err != nil {
		if errors.Is(err, ErrEnvPinsURL) {
			writeJSONBridge(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		writeJSONBridge(w, http.StatusInternalServerError, map[string]string{"error": "unavailable"})
		return
	}
	writeJSONBridge(w, http.StatusOK, c.Snapshot())
}

func (c *CloudAccess) handleReset(w http.ResponseWriter, _ *http.Request) {
	if err := c.Reset(); err != nil {
		writeJSONBridge(w, http.StatusInternalServerError, map[string]string{"error": "unavailable"})
		return
	}
	writeJSONBridge(w, http.StatusOK, c.Snapshot())
}

func writeJSONBridge(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
