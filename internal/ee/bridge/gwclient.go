// gwclient.go is the bridge side of the entry-gateway channel (product
// design §2.3/§3.2): enrollment with a locally generated identity, opaque
// short-TTL access tokens with renewal, config pulls applied as the
// managed-settings overlay, and heartbeats. The pulled config is the
// cache: it lands in the settings database (encrypted secrets included)
// and keeps the bridge running when the gateway is unreachable — the
// status surfaces a STALE flag instead of clearing anything.
package bridge

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/projanvil/woca/internal/platform/authn"
	"github.com/projanvil/woca/internal/platform/machineid"
	"github.com/projanvil/woca/internal/settings"
)

// Gateway cadences: poll/heartbeat interval, token renewal margin, and the
// STALE threshold (no successful pull for this long marks the config
// stale — the cache stays authoritative).
const (
	gwInterval    = 30 * time.Second
	gwRenewMargin = 30 * time.Minute
	gwStaleAfter  = 5 * time.Minute
)

// GatewayConfig pins the gateway endpoint and enrollment material.
type GatewayConfig struct {
	// URL is the edge access point base URL (the only control-plane
	// address the bridge ever holds, D5).
	URL string
	// EnrollmentToken is the one-time token from the control-plane console.
	EnrollmentToken string
	// StatePath persists the bridge identity + access token (0600) —
	// the file-based persistence (CLI flows); CloudAccess installs the
	// settings-database persistence instead (design §3.3).
	StatePath string
	// Persist overrides the file persistence when set.
	Persist StatePersist
	// Insecure skips TLS verification against the gateway — development
	// 联调 only (self-signed gateways); the management page's
	// allow_insecure toggle declares the matching server posture.
	Insecure bool
	// PinSource supplies the pinned edge SPKI (TOFU, design §6.5): when
	// it returns a non-empty pin, a mismatching server certificate is a
	// hard failure.
	PinSource func() string
	// OnPin records the observed SPKI on the first verified handshake.
	OnPin func(pin string)
}

// StatePersist stores the bridge machine identity. The concrete shape is
// opaque on purpose: file-based (CLI) and settings-database (serve)
// persistences implement it.
type StatePersist interface {
	Load() (stateFile, error)
	Save(stateFile) error
}

// GatewayClient is the bridge's outbound channel to the control plane.
type GatewayClient struct {
	cfg GatewayConfig
	hc  *http.Client
	log zerolog.Logger

	mu          sync.Mutex
	bridgeID    string
	access      string
	accessExp   int64
	privateKey  ed25519.PrivateKey
	lastPull    time.Time
	lastVersion string
	stale       bool

	// Apply installs a pulled config as the managed overlay.
	Apply func(settings map[string]string) error
	// Status snapshot for the status page.
	StatusFn func() json.RawMessage
	// OnEnrolled fires once after a successful enrollment (CloudAccess
	// consumes the one-time token key there).
	OnEnrolled func()
}

// PinnedTransport builds the shared edge transport: the dev posture may
// skip chain validation, but the TOFU pin — when one is on record — is
// enforced regardless (Insecure opts out of CA checks, not identity).
// Shared by the gateway channel and the tunnel so both hold the same
// trust in the edge.
func PinnedTransport(insecure bool, pinSource func() string, onPin func(string)) http.RoundTripper {
	t := http.DefaultTransport.(*http.Transport).Clone()
	tlsCfg := &tls.Config{InsecureSkipVerify: insecure} //nolint:gosec // explicit dev toggle
	if pinSource != nil {
		tlsCfg.VerifyConnection = func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return nil
			}
			sum := sha256.Sum256(cs.PeerCertificates[0].RawSubjectPublicKeyInfo)
			pin := hex.EncodeToString(sum[:])
			if pinned := pinSource(); pinned != "" {
				if pinned != pin {
					return fmt.Errorf("bridge: edge certificate pin mismatch (TOFU)")
				}
				return nil
			}
			if onPin != nil {
				onPin(pin)
			}
			return nil
		}
	}
	t.TLSClientConfig = tlsCfg
	return t
}

// NewGatewayClient builds the channel client.
func NewGatewayClient(cfg GatewayConfig, log zerolog.Logger) *GatewayClient {
	transport := http.DefaultTransport
	if cfg.Insecure || cfg.PinSource != nil {
		transport = PinnedTransport(cfg.Insecure, cfg.PinSource, cfg.OnPin)
	}
	return &GatewayClient{cfg: cfg, log: log, hc: &http.Client{Timeout: 20 * time.Second, Transport: transport}}
}

// WithLogger swaps the channel logger (the serve path installs its
// structured logger once available).
func (g *GatewayClient) WithLogger(log zerolog.Logger) { g.log = log }

// SetAccessTokenForTest arms the machine token for handler-stub tests
// (production tokens arrive via enrollment).
func (g *GatewayClient) SetAccessTokenForTest(token string) {
	g.mu.Lock()
	g.access = token
	g.mu.Unlock()
}

// stateFile is the persisted identity shape.
type stateFile struct {
	BridgeID     string `json:"bridge_id"`
	PrivateKey   []byte `json:"private_key"`
	AccessToken  string `json:"access_token,omitempty"`
	AccessExpiry int64  `json:"access_expires_at,omitempty"`
}

// Enroll registers this bridge with the gateway: a local Ed25519 identity
// is generated once (the private key never leaves the device) and the
// one-time enrollment token is exchanged for bridge id + access token.
func (g *GatewayClient) Enroll(ctx context.Context) error {
	if !machineid.ValidEnrollment(g.cfg.EnrollmentToken) {
		return fmt.Errorf("bridge: invalid enrollment token shape")
	}
	st, err := g.loadState()
	if err != nil {
		return err
	}
	priv := ed25519.PrivateKey(st.PrivateKey)
	if priv == nil {
		if priv, err = machineid.GenerateIdentity(); err != nil {
			return err
		}
	}
	pub, err := machineid.PublicKeyPEM(priv.Public().(ed25519.PublicKey))
	if err != nil {
		return err
	}
	// k3s-mode tokens carry the edge's SPKI pin (design §2.3): seed it
	// before the first dial so the enrollment handshake itself verifies
	// the edge — the one-time credential never travels to a lookalike.
	// A pin that contradicts the one already on record is a hard reject.
	base, tokenPin := machineid.SplitEnrollment(g.cfg.EnrollmentToken)
	if tokenPin != "" {
		if g.cfg.PinSource != nil {
			if existing := g.cfg.PinSource(); existing != "" && existing != tokenPin {
				return fmt.Errorf("bridge: enrollment token pins a different edge certificate (refusing)")
			}
		}
		if g.cfg.OnPin != nil {
			g.cfg.OnPin(tokenPin)
		}
	}
	body, _ := json.Marshal(map[string]any{
		"enrollment_token": base,
		"public_key":       pub,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.cfg.URL+"/gw/enroll", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.hc.Do(req)
	if err != nil {
		return fmt.Errorf("bridge: enroll: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("bridge: enroll: gateway returned %d", resp.StatusCode)
	}
	var out struct {
		BridgeID     string `json:"bridge_id"`
		AccessToken  string `json:"access_token"`
		AccessExpiry int64  `json:"access_expires_at"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<10)).Decode(&out); err != nil {
		return err
	}
	g.mu.Lock()
	g.bridgeID = out.BridgeID
	g.access = out.AccessToken
	g.accessExp = out.AccessExpiry
	g.privateKey = priv
	g.mu.Unlock()
	if err := g.saveState(); err != nil {
		return err
	}
	// Spend the one-time token only once the identity is durably saved:
	// the server already consumed it, so losing it before persistence
	// would leave the bridge unenrollable without a fresh token.
	if g.OnEnrolled != nil {
		g.OnEnrolled()
	}
	return nil
}

// RequestEnrollment runs the worker-led flow (product design §2.3 备选):
// register this machine's key, print the fingerprint for the
// administrator, then poll until the console approves — no token pasting.
func (g *GatewayClient) RequestEnrollment(ctx context.Context, onFingerprint func(fp string)) error {
	if g.privateKey == nil {
		if err := g.resume(); err != nil || g.privateKey == nil {
			priv, err := machineid.GenerateIdentity()
			if err != nil {
				return err
			}
			g.mu.Lock()
			g.privateKey = priv
			g.mu.Unlock()
			if err := g.saveState(); err != nil {
				return err
			}
		}
	}
	g.mu.Lock()
	priv := g.privateKey
	g.mu.Unlock()
	pub, err := machineid.PublicKeyPEM(priv.Public().(ed25519.PublicKey))
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]any{"public_key": pub})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.cfg.URL+"/gw/enroll-request", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.hc.Do(req)
	if err != nil {
		return fmt.Errorf("bridge: enrollment request: %w", err)
	}
	var out struct {
		Fingerprint string `json:"fingerprint"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 4<<10)).Decode(&out)
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted || out.Fingerprint == "" {
		return fmt.Errorf("bridge: enrollment request: gateway returned %d", resp.StatusCode)
	}
	if onFingerprint != nil {
		onFingerprint(out.Fingerprint)
	}
	// Poll for the console approval until ctx ends.
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		req2, err := http.NewRequestWithContext(ctx, http.MethodGet,
			g.cfg.URL+"/gw/enroll-status?fingerprint="+out.Fingerprint, nil)
		if err != nil {
			return err
		}
		resp2, err := g.hc.Do(req2)
		if err != nil {
			return err
		}
		if resp2.StatusCode == http.StatusOK {
			var approved struct {
				EnrollmentToken string `json:"enrollment_token"`
			}
			_ = json.NewDecoder(io.LimitReader(resp2.Body, 4<<10)).Decode(&approved)
			resp2.Body.Close()
			g.cfg.EnrollmentToken = approved.EnrollmentToken
			return g.Enroll(ctx)
		}
		resp2.Body.Close()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

// ConfigPayload is the config-plane pull response.
type ConfigPayload struct {
	Version  string            `json:"version"`
	Settings map[string]string `json:"settings"`
}

// Pull fetches the config payload (304 on unchanged version yields
// (nil, nil)); every failure keeps the local cache untouched (§3.2).
func (g *GatewayClient) Pull(ctx context.Context) (*ConfigPayload, error) {
	g.mu.Lock()
	token, version := g.access, g.lastVersion
	resumed := g.bridgeID != ""
	g.mu.Unlock()
	if token == "" {
		// Lazily resume a persisted identity once (Run does this eagerly;
		// direct Pull callers get the same treatment).
		if !resumed {
			if err := g.resume(); err == nil {
				g.mu.Lock()
				token, version = g.access, g.lastVersion
				g.mu.Unlock()
			}
		}
		if token == "" {
			return nil, fmt.Errorf("bridge: gateway channel not enrolled")
		}
	}
	url := g.cfg.URL + "/gw/config?version=" + version
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := g.hc.Do(req)
	if err != nil {
		g.markStale(err)
		return nil, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotModified:
		g.mu.Lock()
		g.lastPull = time.Now()
		g.stale = false
		g.mu.Unlock()
		return nil, nil
	case resp.StatusCode == http.StatusOK:
	default:
		err := fmt.Errorf("bridge: config pull: gateway returned %d", resp.StatusCode)
		g.markStale(err)
		return nil, err
	}
	var out ConfigPayload
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		// 解析失败绝不清缓存：坏包只标记 STALE，旧配置继续生效。
		g.markStale(err)
		return nil, err
	}
	g.mu.Lock()
	g.lastPull = time.Now()
	g.lastVersion = out.Version
	g.stale = false
	g.mu.Unlock()
	return &out, nil
}

// PullWait is the blocking-query form of Pull (product design §2.3: the
// push channel is the long poll — a config change returns immediately, an
// unchanged config parks until the wait elapses and yields (nil, nil)).
func (g *GatewayClient) PullWait(ctx context.Context, wait time.Duration) (*ConfigPayload, error) {
	g.mu.Lock()
	token, version := g.access, g.lastVersion
	resumed := g.bridgeID != ""
	g.mu.Unlock()
	if token == "" {
		if !resumed {
			_ = g.resume()
			g.mu.Lock()
			token, version = g.access, g.lastVersion
			g.mu.Unlock()
		}
		if token == "" {
			return nil, fmt.Errorf("bridge: gateway channel not enrolled")
		}
	}
	url := g.cfg.URL + "/gw/config?version=" + version + "&wait=" + time.Duration.String(wait)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	// The long-poll client outlives the wait window but keeps the pinned
	// (or explicitly insecure) transport — TOFU must hold on the steady
	// pull path, and self-signed 联调 edges must answer long polls too.
	hc := &http.Client{Timeout: wait + 15*time.Second, Transport: g.hc.Transport}
	resp, err := hc.Do(req)
	if err != nil {
		g.markStale(err)
		return nil, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotModified:
		g.mu.Lock()
		g.lastPull = time.Now()
		g.stale = false
		g.mu.Unlock()
		return nil, nil
	case resp.StatusCode == http.StatusOK:
	default:
		err := fmt.Errorf("bridge: config pull: gateway returned %d", resp.StatusCode)
		g.markStale(err)
		return nil, err
	}
	var out ConfigPayload
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		// 解析失败绝不清缓存：坏包只标记 STALE，旧配置继续生效。
		g.markStale(err)
		return nil, err
	}
	g.mu.Lock()
	g.lastPull = time.Now()
	g.lastVersion = out.Version
	g.stale = false
	g.mu.Unlock()
	return &out, nil
}

// Heartbeat ships one status beat to the service plane.
func (g *GatewayClient) Heartbeat(ctx context.Context, status json.RawMessage) error {
	g.mu.Lock()
	token := g.access
	g.mu.Unlock()
	if token == "" {
		return fmt.Errorf("bridge: gateway channel not enrolled")
	}
	body, _ := json.Marshal(status)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.cfg.URL+"/gw/heartbeat", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := g.hc.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("bridge: heartbeat: gateway returned %d", resp.StatusCode)
	}
	return nil
}

// CoreSession is the identity the private core returns for a login or
// introspection (product design v1.8 §2.1/§2.7): the bridge holds no
// user credentials — this is a validation RESULT, never key material.
type CoreSession struct {
	Token     string `json:"token,omitempty"`
	KeyPrefix string `json:"key_prefix"`
	TenantID  string `json:"tenant_id"`
	UserID    string `json:"user_id"`
	Remark    string `json:"remark"`
	Role      string `json:"role"`
	ExpiresAt int64  `json:"expires_at"`
	// Revoked marks an introspection failure with the enumerated code
	// (the client contract the 401 body and SSE close event carry).
	Revoked bool `json:"revoked,omitempty"`
	// Code carries the core's enumerated failure code on a refused login.
	Code string `json:"code,omitempty"`
}

// AuthLogin forwards a user login to the private core over the machine
// channel (scenario three, §2.7): the core validates the woca_ key and
// issues the opaque session; the bridge never sees key material beyond
// this one forwarding hop. A transport failure is ErrCoreUnreachable —
// the caller fails closed for new logins.
var ErrCoreUnreachable = errors.New("bridge: private core unreachable")

// ErrCoreLoginRevoked marks a login refused with the enumerated revoked
// code (the client contract the 401 body carries, preserved across the
// forwarding hop).
var ErrCoreLoginRevoked = errors.New("bridge: core login refused: revoked")

func (g *GatewayClient) AuthLogin(ctx context.Context, apiKey string) (CoreSession, error) {
	var out CoreSession
	status, err := g.authPOST(ctx, "/gw/auth/login", map[string]string{"api_key": apiKey}, &out)
	if err != nil {
		if status == http.StatusUnauthorized {
			if out.Code == "revoked" {
				return CoreSession{}, ErrCoreLoginRevoked
			}
			return CoreSession{}, authn.ErrUnauthenticated
		}
		return CoreSession{}, err
	}
	if out.Token == "" {
		return CoreSession{}, fmt.Errorf("bridge: core login returned no session")
	}
	return out, nil
}

// FetchedPreset is one catalog entry the config plane serves to bridges
// (§16 pull model: the list never carries credentials — the API key
// arrives only inside the verify authorization).
type FetchedPreset struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Provider    string  `json:"provider"`
	Model       string  `json:"model"`
	MaxTokens   int64   `json:"max_tokens"`
	Temperature float64 `json:"temperature"`
	BaseURL     string  `json:"base_url"`
	// APIKey is set only on a successful verify (the credential rides
	// the machine-authenticated channel; never a client surface).
	APIKey    string `json:"api_key,omitempty"`
	UpdatedAt int64  `json:"updated_at,omitempty"`
}

// FetchModels pulls the operator-authored model catalog from the config
// plane (the bridge's selection surface is exactly this list — empty
// means the deployment offers no models and the frontend must refuse to
// send).
func (g *GatewayClient) FetchModels(ctx context.Context) ([]FetchedPreset, error) {
	g.mu.Lock()
	token := g.access
	g.mu.Unlock()
	if token == "" {
		return nil, fmt.Errorf("bridge: gateway channel not enrolled")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.cfg.URL+"/gw/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := g.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrCoreUnreachable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("bridge: fetch models: status %d", resp.StatusCode)
	}
	var out []FetchedPreset
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// VerifyModel asks the config plane to authorize the preferred model
// (preset id, or the provider+model pair): a verified model returns the
// full preset including its API key; an unknown model is an error and
// the run must be refused (product ruling — the catalog authority
// decides).
func (g *GatewayClient) VerifyModel(ctx context.Context, id, provider, model string) (FetchedPreset, error) {
	var out FetchedPreset
	status, err := g.authPOST(ctx, "/gw/models/verify", map[string]string{"id": id, "provider": provider, "model": model}, &out)
	if err != nil {
		if status == http.StatusNotFound {
			return FetchedPreset{}, ErrUnknownModel
		}
		return FetchedPreset{}, err
	}
	return out, nil
}

// ErrUnknownModel marks a preferred model the catalog authority refused.
var ErrUnknownModel = errors.New("bridge: unknown model (not in the operator catalog)")

// AuthLogout revokes the session at the core (the authority — the
// bridge's local copy dies either way).
func (g *GatewayClient) AuthLogout(ctx context.Context, token string) error {
	var out map[string]any
	_, err := g.authPOST(ctx, "/gw/auth/logout", map[string]string{"token": token}, &out)
	return err
}

// AuthIntrospect validates a session token against the private core
// (RFC 7662-shaped). An inactive result is NOT an error: Revoked says
// which failure code applies; only transport failures return
// ErrCoreUnreachable (the caller's grace decision point).
func (g *GatewayClient) AuthIntrospect(ctx context.Context, token string) (CoreSession, error) {
	var resp struct {
		Active    bool        `json:"active"`
		Code      string      `json:"code"`
		Session   CoreSession `json:"-"`
		KeyPrefix string      `json:"key_prefix"`
		TenantID  string      `json:"tenant_id"`
		UserID    string      `json:"user_id"`
		Remark    string      `json:"remark"`
		Role      string      `json:"role"`
		ExpiresAt int64       `json:"expires_at"`
	}
	if _, err := g.authPOST(ctx, "/gw/auth/introspect", map[string]string{"token": token}, &resp); err != nil {
		return CoreSession{}, err
	}
	if !resp.Active {
		return CoreSession{Revoked: resp.Code == "revoked"}, nil
	}
	return CoreSession{
		KeyPrefix: resp.KeyPrefix, TenantID: resp.TenantID, UserID: resp.UserID,
		Remark: resp.Remark, Role: resp.Role, ExpiresAt: resp.ExpiresAt,
	}, nil
}

// authPOST posts JSON to a core-internal auth route with the machine
// token; non-2xx answers are protocol errors, transport errors are
// ErrCoreUnreachable.
func (g *GatewayClient) authPOST(ctx context.Context, path string, body any, out any) (int, error) {
	g.mu.Lock()
	token := g.access
	g.mu.Unlock()
	if token == "" {
		return 0, fmt.Errorf("bridge: gateway channel not enrolled")
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.cfg.URL+path, bytes.NewReader(raw))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := g.hc.Do(req)
	if err != nil {
		return 0, fmt.Errorf("%w: %s", ErrCoreUnreachable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Decode the failure body too: the enumerated code is the client
		// contract (revoked) and must survive the hop.
		_ = json.NewDecoder(io.LimitReader(resp.Body, 4<<10)).Decode(out)
		return resp.StatusCode, fmt.Errorf("bridge: core auth %s: status %d", path, resp.StatusCode)
	}
	return resp.StatusCode, json.NewDecoder(io.LimitReader(resp.Body, 16<<10)).Decode(out)
}

// Renew exchanges the live access token for a fresh one.
func (g *GatewayClient) Renew(ctx context.Context) error {
	g.mu.Lock()
	token := g.access
	g.mu.Unlock()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.cfg.URL+"/gw/renew", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := g.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("bridge: renew: gateway returned %d", resp.StatusCode)
	}
	var out struct {
		AccessToken  string `json:"access_token"`
		AccessExpiry int64  `json:"access_expires_at"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<10)).Decode(&out); err != nil {
		return err
	}
	g.mu.Lock()
	g.access = out.AccessToken
	g.accessExp = out.AccessExpiry
	g.mu.Unlock()
	return g.saveState()
}

// Run drives the channel until ctx ends: an initial blocking pull, then a
// steady loop of config pulls, heartbeats, and token renewal. The loop is
// failure-tolerant by design — gateway outages mark STALE and never stop
// the bridge.
func (g *GatewayClient) Run(ctx context.Context, st *settings.Store) {
	if st != nil && g.Apply == nil {
		g.Apply = func(kv map[string]string) error { return applyManaged(st, kv) }
	}
	// Resume an enrolled state if present.
	if err := g.resume(); err != nil {
		g.log.Warn().Err(err).Msg("bridge: gateway state resume failed")
	}
	if !g.enrolled() {
		if g.cfg.EnrollmentToken == "" {
			g.log.Info().Msg("bridge: no gateway configured — running standalone")
			return
		}
		if err := g.Enroll(ctx); err != nil {
			g.log.Error().Err(err).Msg("bridge: gateway enrollment failed (run `woca-bridge gateway enroll` with a fresh token)")
			return
		}
		g.log.Info().Str("bridge", g.bridgeID).Msg("bridge: enrolled with the entry gateway")
	}
	if cfg, err := g.Pull(ctx); err != nil {
		g.log.Warn().Err(err).Msg("bridge: initial config pull failed; running on cache")
	} else if cfg != nil {
		g.applyConfig(cfg)
	}
	// Beat once at boot: the registry's live window gates edge routing,
	// so a freshly enrolled bridge must not wait out the first interval
	// before web traffic can reach it.
	g.heartbeatOnce(ctx)
	tick := time.NewTicker(gwInterval)
	defer tick.Stop()
	for {
		// The config channel is a blocking query: park up to gwInterval
		// waiting for a version change; a push returns immediately. The
		// park comes first so a change right after boot lands instantly
		// instead of waiting out a whole interval.
		if cfg, err := g.PullWait(ctx, gwInterval); err != nil {
			g.log.Warn().Err(err).Msg("bridge: config poll failed (cache stays authoritative)")
		} else if cfg != nil {
			g.applyConfig(cfg)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			g.renewIfNeeded(ctx)
			g.heartbeatOnce(ctx)
		}
	}
}

func (g *GatewayClient) renewIfNeeded(ctx context.Context) {
	g.mu.Lock()
	exp := g.accessExp
	g.mu.Unlock()
	if exp != 0 && time.Until(time.UnixMilli(exp)) < gwRenewMargin {
		if err := g.Renew(ctx); err != nil {
			g.log.Warn().Err(err).Msg("bridge: token renewal failed")
		}
	}
}

func (g *GatewayClient) heartbeatOnce(ctx context.Context) {
	status := json.RawMessage(`{}`)
	if g.StatusFn != nil {
		if s := g.StatusFn(); len(s) > 0 {
			status = s
		}
	}
	if err := g.Heartbeat(ctx, status); err != nil {
		g.log.Warn().Err(err).Msg("bridge: heartbeat failed")
	}
}

func (g *GatewayClient) applyConfig(cfg *ConfigPayload) {
	if g.Apply == nil {
		return
	}
	// §3.2 red line: an empty payload never clears the cache. A config
	// plane that legitimately manages nothing sends no new version at
	// all; an empty 200 is a fault shape (Claude Code #70181 lesson) —
	// keep the overlay and flag STALE instead.
	if len(cfg.Settings) == 0 {
		err := fmt.Errorf("bridge: config payload carried no settings (version %s)", cfg.Version)
		g.markStale(err)
		return
	}
	if err := g.Apply(cfg.Settings); err != nil {
		g.log.Error().Err(err).Msg("bridge: applying managed config failed (dropping offending keys)")
		return
	}
	g.log.Info().Str("version", cfg.Version).Msg("bridge: managed config applied")
}

func (g *GatewayClient) markStale(err error) {
	g.mu.Lock()
	g.stale = true
	g.mu.Unlock()
	g.log.Warn().Err(err).Msg("bridge: config marked STALE (cache stays authoritative)")
}

// GatewayStatus is the status-page view of the channel.
type GatewayStatus struct {
	Enrolled    bool   `json:"enrolled"`
	BridgeID    string `json:"bridge_id,omitempty"`
	LastVersion string `json:"last_version,omitempty"`
	LastPullISO string `json:"last_pull,omitempty"`
	Stale       bool   `json:"stale"`
}

// Status snapshots the channel state.
func (g *GatewayClient) Status() GatewayStatus {
	g.mu.Lock()
	defer g.mu.Unlock()
	st := GatewayStatus{
		Enrolled:    g.access != "",
		BridgeID:    g.bridgeID,
		LastVersion: g.lastVersion,
		Stale:       g.stale,
	}
	if !g.lastPull.IsZero() {
		st.LastPullISO = g.lastPull.Format(time.RFC3339)
	}
	return st
}

func (g *GatewayClient) enrolled() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.access != ""
}

// resume reloads a persisted state (identity + live access token).
func (g *GatewayClient) resume() error {
	st, err := g.loadState()
	if err != nil || st.BridgeID == "" {
		return err
	}
	g.mu.Lock()
	g.bridgeID = st.BridgeID
	g.privateKey = st.PrivateKey
	if time.Now().UnixMilli() < st.AccessExpiry {
		g.access = st.AccessToken
		g.accessExp = st.AccessExpiry
	}
	g.mu.Unlock()
	return nil
}

// CurrentToken exposes the live access token (the tunnel rides the same
// machine identity).
func (g *GatewayClient) CurrentToken() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.access
}

func (g *GatewayClient) loadState() (stateFile, error) {
	if g.cfg.Persist != nil {
		return g.cfg.Persist.Load()
	}
	var st stateFile
	data, err := os.ReadFile(g.cfg.StatePath)
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return st, fmt.Errorf("bridge: gateway state: %w", err)
	}
	if len(st.PrivateKey) > 0 {
		if len(st.PrivateKey) != ed25519.PrivateKeySize {
			return stateFile{}, fmt.Errorf("bridge: gateway state: bad key size")
		}
		st.PrivateKey = ed25519.PrivateKey(st.PrivateKey)
	}
	return st, nil
}

func (g *GatewayClient) saveState() error {
	g.mu.Lock()
	st := stateFile{
		BridgeID:     g.bridgeID,
		PrivateKey:   []byte(g.privateKey),
		AccessToken:  g.access,
		AccessExpiry: g.accessExp,
	}
	g.mu.Unlock()
	if st.BridgeID == "" {
		return nil
	}
	if g.cfg.Persist != nil {
		return g.cfg.Persist.Save(st)
	}
	if err := os.MkdirAll(filepath.Dir(g.cfg.StatePath), 0o700); err != nil {
		return err
	}
	data, _ := json.Marshal(st)
	// 0600: the access token is a bearer secret bound to this machine.
	return os.WriteFile(g.cfg.StatePath, data, 0o600)
}

// ErrLayerViolation marks a config-plane push that tried to write
// bridge-local infrastructure keys: the two configuration layers never
// mix (§3.3), so the whole package is rejected and audited rather than
// partially applied.
var ErrLayerViolation = errors.New("bridge: config plane pushed infrastructure keys (layer violation)")

// applyManaged converts a pulled key/value map into the managed overlay.
// Infrastructure keys (the bridge-local edge.* layer) reject the package
// up front; unknown-but-innocuous keys are stripped with a warning —
// single-field isolation, §3.2.
func applyManaged(st *settings.Store, kv map[string]string) error {
	for k := range kv {
		if strings.HasPrefix(k, "edge.") {
			return fmt.Errorf("%w: %s", ErrLayerViolation, k)
		}
	}
	// Operator-delivered skills & MCP materialize as files under the
	// managed root (the lowest merge layer; the user's own config wins).
	// Removals propagate too: the delivered set is the full inventory, so
	// entries no longer present disappear on the next apply.
	if raw, ok := kv["managed-skills"]; ok {
		if err := materializeManagedSkills(raw); err != nil {
			return fmt.Errorf("bridge: managed skills: %w", err)
		}
		delete(kv, "managed-skills")
	}
	if raw, ok := kv["managed-mcp"]; ok {
		if err := materializeManagedMCP(raw); err != nil {
			return fmt.Errorf("bridge: managed mcp: %w", err)
		}
		delete(kv, "managed-mcp")
	}
	cfg := settings.Settings{}
	// The managed vocabulary: present-but-empty values are legitimate
	// (an unconfigured domain stays unconfigured) — membership, not
	// usage, decides what counts as unknown below.
	known := map[string]bool{}
	set := func(key string, apply func(v string)) {
		known[key] = true
		if v, ok := kv[key]; ok && strings.TrimSpace(v) != "" {
			apply(v)
		}
	}
	set("provider", func(v string) { cfg.Provider = v })
	set("model", func(v string) { cfg.Model = v })
	set("anthropic-base-url", func(v string) { cfg.AnthropicBaseURL = v })
	set("openai-base-url", func(v string) { cfg.OpenAIBaseURL = v })
	set("glm-base-url", func(v string) { cfg.GLMBaseURL = v })
	set("deepseek-base-url", func(v string) { cfg.DeepSeekBaseURL = v })
	set("checkpoint-backend", func(v string) { cfg.CheckpointBackend = v })
	set("memory-backend", func(v string) { cfg.MemoryBackend = v })
	set("memory-engine", func(v string) { cfg.MemoryEngine = v })
	set("model-whitelist", func(v string) { cfg.ModelWhitelist = v })
	set("max-tokens", func(v string) { fmt.Sscanf(v, "%d", &cfg.MaxTokens) })
	// Private-cloud storage/memory wiring (§3 storage domain): the split
	// postgres coordinates and the remote ladyM endpoint ride the overlay;
	// the passwords ride the encrypted secret store. Without them a
	// "backend: postgres / engine: remote" delivery leaves the bridge with
	// nowhere to connect and every run degrades to local state.
	set("checkpoint-pg-host", func(v string) { cfg.CheckpointPGHost = v })
	set("checkpoint-pg-port", func(v string) { cfg.CheckpointPGPort = v })
	set("checkpoint-pg-db", func(v string) { cfg.CheckpointPGDB = v })
	set("checkpoint-pg-user", func(v string) { cfg.CheckpointPGUser = v })
	set("memory-pg-host", func(v string) { cfg.MemoryPGHost = v })
	set("memory-pg-port", func(v string) { cfg.MemoryPGPort = v })
	set("memory-pg-db", func(v string) { cfg.MemoryPGDB = v })
	set("memory-pg-user", func(v string) { cfg.MemoryPGUser = v })
	set("ladym-endpoint", func(v string) { cfg.LadyMEndpoint = v })
	set("ladym-api-username", func(v string) { cfg.LadyMAPIUsername = v })
	// The enterprise plugin set: a JSON array pinning the enabled
	// built-ins (absent = user-managed; an org can pin a minimum set, not
	// force plugins off — see managedState.applyOverlay).
	set("builtin-plugins", func(v string) {
		var list []string
		if err := json.Unmarshal([]byte(v), &list); err == nil {
			cfg.BuiltinPlugins = list
		}
	})
	st.SetManaged(cfg)
	// Provider API keys are secrets: they land in the encrypted store,
	// never in the overlay struct (R4 — they do not travel past the
	// bridge's server process).
	for key, secret := range map[string]string{
		"anthropic-api-key":      settings.SecretAnthropicAPIKey,
		"openai-api-key":         settings.SecretOpenAIAPIKey,
		"glm-api-key":            settings.SecretGLMAPIKey,
		"deepseek-api-key":       settings.SecretDeepSeekAPIKey,
		"checkpoint-pg-password": settings.SecretCheckpointPGPassword,
		"memory-pg-password":     settings.SecretMemoryPGPassword,
		"ladym-api-password":     settings.SecretLadyMAPIPassword,
	} {
		known[key] = true
		if v := strings.TrimSpace(kv[key]); v != "" {
			if err := st.SetSecret(secret, v); err != nil {
				return fmt.Errorf("bridge: managed secret %s: %w", key, err)
			}
		}
	}
	var unknown []string
	for k := range kv {
		if !known[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		return fmt.Errorf("bridge: dropped non-managed keys: %v", unknown)
	}
	return nil
}

// managedRootPath mirrors agentbuild.ManagedRoot (that package stays
// vocabulary-free; this local copy keeps the seam).
func managedRootPath() string {
	if h := os.Getenv("WOCA_HOME"); h != "" {
		return filepath.Join(h, "managed")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".woca", "managed")
}

// materializeManagedSkills writes the delivered SKILL.md set (frontmatter
// name+description + body) and prunes delivered skills no longer present.
// Delivered set == full inventory: a skill the operator deleted vanishes
// here; a same-name user/workspace skill is untouched (different roots).
func materializeManagedSkills(raw string) error {
	var list []struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Body        string `json:"body"`
	}
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		// Bad delivery: keep the previous materialized set entirely
		// (§3.2 — a broken package never wipes local state).
		return nil
	}
	root := managedRootPath()
	if root == "" {
		return nil
	}
	dir := filepath.Join(root, "skills")
	if len(list) == 0 {
		return os.RemoveAll(dir)
	}
	want := map[string]bool{}
	for _, sk := range list {
		name := filepath.Base(sk.Name)
		if name == "" || name == "." || strings.ContainsAny(name, "/\\") {
			continue
		}
		skillDir := filepath.Join(dir, name)
		if err := os.MkdirAll(skillDir, 0o755); err != nil {
			return err
		}
		var b strings.Builder
		b.WriteString("---\nname: " + name + "\n")
		if sk.Description != "" {
			b.WriteString("description: " + strings.ReplaceAll(sk.Description, "\n", " ") + "\n")
		}
		b.WriteString("---\n\n")
		b.WriteString(sk.Body)
		if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(b.String()), 0o644); err != nil {
			return err
		}
		want[name] = true
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	for _, e := range entries {
		if e.IsDir() && !want[e.Name()] {
			_ = os.RemoveAll(filepath.Join(dir, e.Name()))
		}
	}
	return nil
}

// materializeManagedMCP writes the delivered MCP specs as the managed
// root's mcp.json (claudeenv.LoadMCPConfig shape: {"mcpServers":{...}} is
// the import format; the layered merger consumes name=spec strings, so
// the config-plane grammar stores plain specs and this writes the
// simplest carrier the loader understands).
func materializeManagedMCP(raw string) error {
	var specs []string
	if err := json.Unmarshal([]byte(raw), &specs); err != nil {
		return nil // §3.2: keep the previous set on a bad delivery
	}
	root := managedRootPath()
	if root == "" {
		return nil
	}
	if len(specs) == 0 {
		_ = os.Remove(filepath.Join(root, "mcp.json"))
		return nil
	}
	doc := map[string]any{"mcpServers": map[string]any{}}
	servers := doc["mcpServers"].(map[string]any)
	for _, spec := range specs {
		name, rest, ok := strings.Cut(spec, "=")
		if !ok {
			continue
		}
		rest = strings.TrimSpace(rest)
		if strings.HasPrefix(rest, "http://") || strings.HasPrefix(rest, "https://") {
			servers[name] = map[string]any{"type": "http", "url": rest}
		} else {
			// A command spec: run via sh -c (the merger's grammar).
			servers[name] = map[string]any{"type": "stdio", "command": "sh", "args": []string{"-c", rest}}
		}
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	out, _ := json.MarshalIndent(doc, "", "  ")
	return os.WriteFile(filepath.Join(root, "mcp.json"), out, 0o644)
}
