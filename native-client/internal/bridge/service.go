// Package bridge exposes the small native bootstrap surface that is bound to
// the Wails frontend. Authentication details remain behind auth.Activator.
package bridge

import (
	"context"
	"errors"
	"net"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/projanvil/woca/native-client/internal/auth"
	"github.com/projanvil/woca/native-client/internal/configstore"
	"github.com/projanvil/woca/native-client/internal/transport"
)

type State string

const (
	StateUnconfigured        State = "unconfigured"
	StateAuthorizationNeeded State = "authorization_required"
	StateReady               State = "ready"
	StateFatalLocalData      State = "fatal_local_data"
)

type BootstrapState struct {
	State        State  `json:"state"`
	ServerURL    string `json:"server_url,omitempty"`
	Message      string `json:"message,omitempty"`
	DatabasePath string `json:"database_path,omitempty"`
	// BridgeID is the active profile's target bridge (edge entries,
	// v1.6 entry ②; "" for direct/embedded profiles).
	BridgeID string `json:"bridge_id,omitempty"`
	// EmbeddedURL exposes the embedded bridge's base URL so the connect
	// page can offer entry ① (wails only; "" when not running).
	EmbeddedURL string `json:"embedded_url,omitempty"`
}

type ConnectionInput struct {
	ServerURL string `json:"server_url"`
	APIKey    string `json:"api_key"`
	// BridgeID selects the target bridge when ServerURL is an edge
	// access point (optional: the edge auto-routes single-bridge setups).
	BridgeID string `json:"bridge_id,omitempty"`
}

type PreferenceInput struct {
	Key      string `json:"key"`
	Value    string `json:"value"`
	Revision uint64 `json:"revision"`
}

type PreferenceValue struct {
	Value string `json:"value"`
	Found bool   `json:"found"`
}

type PreferenceWriteResult struct {
	Revision uint64 `json:"revision"`
	Applied  bool   `json:"applied"`
}

type nativeStore interface {
	SaveProfile(context.Context, string) (configstore.Profile, error)
	ActiveProfile(context.Context) (configstore.Profile, bool, error)
	SetProfileBridge(context.Context, string, string) error
	ProfileBridge(context.Context, string) string
	SetProfileRelayToken(context.Context, string, string) error
	ProfileRelayToken(context.Context, string) string
	DeleteProfileRelayToken(context.Context, string) error
	ListProfiles(context.Context) ([]configstore.ProfileRow, error)
	ProfileByID(context.Context, int64) (configstore.Profile, bool, error)
	ActivateProfile(context.Context, int64) (configstore.Profile, bool, error)
	DeleteProfile(context.Context, int64) (bool, error)
	PutPreferenceVersioned(context.Context, string, string, uint64) (bool, error)
	Preference(context.Context, string) (string, bool, error)
}

// SessionVault stores the opaque native session: sealed (AES-GCM) in SQLite,
// opened only in memory. A nil vault keeps the M1 scaffold behavior where
// readiness never survives a restart.
type SessionVault interface {
	// Load returns the stored session id when one exists and is unexpired.
	Load(ctx context.Context, profileID int64) (string, bool)
	// Save seals and persists a freshly activated session.
	Save(ctx context.Context, profileID int64, sessionID string, expiresAt int64) error
	// Delete removes the local session.
	Delete(ctx context.Context, profileID int64) error
	// Revoke performs the best-effort remote logout (the local session is
	// deleted regardless of the outcome).
	Revoke(ctx context.Context, baseURL, sessionID string) error
}

// Service retains only the current process's ready marker. The future opaque,
// device-bound session is intentionally not represented here yet.
type Service struct {
	store     nativeStore
	activator auth.Activator
	vault     SessionVault

	mu           sync.RWMutex
	readyBaseURL string
	embeddedURL  string
	// now stamps session-expiry comparisons; tests shrink the clock.
	now func() time.Time
	// relayLogin performs the relay's POST /api/auth/login; injectable
	// for tests and for the TOFU-pinned client (WithRelayLogin).
	relayLogin func(ctx context.Context, baseURL, apiKey string) (transport.RelaySession, error)
	// relayLogout performs the relay's best-effort POST /api/auth/logout
	// before the local token row is deleted (WithRelayLogout).
	relayLogout func(ctx context.Context, baseURL, token string) error
}

func NewService(store nativeStore, activator auth.Activator, opts ...ServiceOption) *Service {
	if activator == nil {
		activator = auth.UnavailableActivator{}
	}
	s := &Service{
		store:     store,
		activator: activator,
		now:       time.Now,
		relayLogin: func(ctx context.Context, baseURL, apiKey string) (transport.RelaySession, error) {
			return transport.RelayLogin(ctx, nil, baseURL, apiKey)
		},
		relayLogout: func(ctx context.Context, baseURL, token string) error {
			return transport.RelayLogout(ctx, nil, baseURL, token)
		},
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// ServiceOption configures optional service capabilities.
type ServiceOption func(*Service)

// WithSessionVault enables persistent opaque sessions.
func WithSessionVault(v SessionVault) ServiceOption {
	return func(s *Service) { s.vault = v }
}

// WithRelayLogin overrides the relay login call — production wires the
// TOFU-pinned client; tests inject a stub.
func WithRelayLogin(fn func(ctx context.Context, baseURL, apiKey string) (transport.RelaySession, error)) ServiceOption {
	return func(s *Service) { s.relayLogin = fn }
}

// WithRelayLogout overrides the relay logout call (best-effort remote
// revocation); tests inject a stub.
func WithRelayLogout(fn func(ctx context.Context, baseURL, token string) error) ServiceOption {
	return func(s *Service) { s.relayLogout = fn }
}

func (s *Service) Bootstrap(ctx context.Context) (BootstrapState, error) {
	profile, configured, err := s.activeProfile(ctx)
	if err != nil {
		return BootstrapState{}, err
	}
	state := BootstrapState{EmbeddedURL: s.embeddedBaseURL()}
	if !configured {
		return BootstrapState{State: StateUnconfigured, EmbeddedURL: state.EmbeddedURL}, nil
	}
	state.ServerURL = profile.BaseURL
	state.BridgeID = s.store.ProfileBridge(ctx, profile.BaseURL)
	if s.isReadyFor(profile.BaseURL) {
		state.State = StateReady
		return state, nil
	}
	// A stored, unexpired session re-opens the app without asking again.
	if s.vault != nil {
		if sessionID, ok := s.vault.Load(ctx, profile.ID); ok && sessionID != "" {
			s.markReady(profile.BaseURL)
			state.State = StateReady
			return state, nil
		}
	}
	// A stored relay session token re-opens the app: relay sessions live
	// server-side (7d sliding + 30d absolute, same table as web
	// sessions); the local copy is a plain preference row mirroring it.
	if s.store.ProfileRelayToken(ctx, profile.BaseURL) != "" {
		s.markReady(profile.BaseURL)
		state.State = StateReady
		return state, nil
	}
	state.State = StateAuthorizationNeeded
	return state, nil
}

func (s *Service) Connect(ctx context.Context, input ConnectionInput) (state BootstrapState, err error) {
	// Clear the local copy before returning. The key is only copied into the
	// one activation request below and is never persisted or logged.
	defer func() { input.APIKey = "" }()

	if strings.TrimSpace(input.APIKey) == "" {
		return BootstrapState{}, errors.New("API key is required")
	}
	if s.store == nil {
		return BootstrapState{}, errors.New("connection profile store is required")
	}

	// A newly selected URL must not inherit readiness from the prior profile.
	s.clearReady()
	profile, err := s.store.SaveProfile(ctx, input.ServerURL)
	if err != nil {
		return BootstrapState{}, err
	}
	if err := s.store.SetProfileBridge(ctx, input.ServerURL, input.BridgeID); err != nil {
		return BootstrapState{}, err
	}
	// A direct-bridge connect supersedes any relay session stored for
	// this URL: the credential chain prefers the relay token, so a stale
	// row would shadow the fresh bridge credentials with 401s (mirrors
	// ConnectRelay clearing the bridge pin).
	if err := s.store.DeleteProfileRelayToken(ctx, input.ServerURL); err != nil {
		return BootstrapState{}, err
	}

	result, err := s.activator.Activate(ctx, auth.ActivationRequest{
		BaseURL:  profile.BaseURL,
		APIKey:   input.APIKey,
		BridgeID: input.BridgeID,
	})
	if err != nil {
		return BootstrapState{State: StateAuthorizationNeeded, ServerURL: profile.BaseURL, BridgeID: input.BridgeID}, err
	}
	if result.Ready {
		if s.vault != nil && result.SessionID != "" {
			if err := s.vault.Save(ctx, profile.ID, result.SessionID, result.ExpiresAt); err != nil {
				return BootstrapState{State: StateAuthorizationNeeded, ServerURL: profile.BaseURL, BridgeID: input.BridgeID}, err
			}
		}
		s.markReady(profile.BaseURL)
		return BootstrapState{State: StateReady, ServerURL: profile.BaseURL, BridgeID: input.BridgeID}, nil
	}
	return BootstrapState{State: StateAuthorizationNeeded, ServerURL: profile.BaseURL, BridgeID: input.BridgeID}, nil
}

// ConnectLoopback adopts the embedded bridge (product design §5.1): the
// loopback bind is trusted, so the profile needs no activation — the
// default experience is zero-login.
func (s *Service) ConnectLoopback(ctx context.Context, baseURL string) (BootstrapState, error) {
	if s.store == nil {
		return BootstrapState{}, errors.New("connection profile store is required")
	}
	if !isLoopbackURL(baseURL) {
		return BootstrapState{}, errors.New("loopback connect requires a loopback URL")
	}
	s.clearReady()
	profile, err := s.store.SaveProfile(ctx, baseURL)
	if err != nil {
		return BootstrapState{}, err
	}
	// The embedded bridge is its own target — no edge routing applies.
	if err := s.store.SetProfileBridge(ctx, baseURL, ""); err != nil {
		return BootstrapState{}, err
	}
	// Loopback is a direct-bridge path: drop any stale relay token for
	// this URL (same shadowing hazard as Connect).
	if err := s.store.DeleteProfileRelayToken(ctx, baseURL); err != nil {
		return BootstrapState{}, err
	}
	s.markReady(profile.BaseURL)
	return BootstrapState{State: StateReady, ServerURL: profile.BaseURL}, nil
}

// ConnectRelay adopts a personal woca-relay profile (design §3.3): the
// wcr_ key logs in at the relay's /api/auth/login, and the response's
// body token is stored via SetProfileRelayToken — that token is what the
// transport's StaticBearer then presents on every request. The key
// itself is cleared from the input copy and never persisted.
func (s *Service) ConnectRelay(ctx context.Context, input ConnectionInput) (BootstrapState, error) {
	defer func() { input.APIKey = "" }()

	if strings.TrimSpace(input.APIKey) == "" {
		return BootstrapState{}, errors.New("relay key is required")
	}
	if s.store == nil {
		return BootstrapState{}, errors.New("connection profile store is required")
	}

	// A newly selected URL must not inherit readiness from the prior profile.
	s.clearReady()
	profile, err := s.store.SaveProfile(ctx, input.ServerURL)
	if err != nil {
		return BootstrapState{}, err
	}
	// Relay profiles are their own target — no edge bridge routing.
	if err := s.store.SetProfileBridge(ctx, input.ServerURL, ""); err != nil {
		return BootstrapState{}, err
	}
	session, err := s.relayLogin(ctx, profile.BaseURL, input.APIKey)
	if err != nil {
		return BootstrapState{State: StateAuthorizationNeeded, ServerURL: profile.BaseURL}, err
	}
	if err := s.store.SetProfileRelayToken(ctx, profile.BaseURL, session.Token); err != nil {
		return BootstrapState{State: StateAuthorizationNeeded, ServerURL: profile.BaseURL}, err
	}
	s.markReady(profile.BaseURL)
	return BootstrapState{State: StateReady, ServerURL: profile.BaseURL}, nil
}

// connect page's entry ① needs it whenever the user re-connects).
func (s *Service) SetEmbeddedBaseURL(rawURL string) {
	s.mu.Lock()
	s.embeddedURL = rawURL
	s.mu.Unlock()
}

func (s *Service) embeddedBaseURL() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.embeddedURL
}

// ConnectEmbedded is entry ① of the connection page (design §6.6): adopt
// the embedded bridge — auto-launched, loopback-trusted, no inputs.
func (s *Service) ConnectEmbedded(ctx context.Context) (BootstrapState, error) {
	base := s.embeddedBaseURL()
	if base == "" {
		return BootstrapState{}, errors.New("the embedded bridge is not running")
	}
	state, err := s.ConnectLoopback(ctx, base)
	state.EmbeddedURL = base
	return state, err
}

// isLoopbackURL reports whether the URL host is a loopback address.
func isLoopbackURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := u.Hostname()
	return host == "localhost" || net.ParseIP(host).IsLoopback()
}

func (s *Service) Logout(ctx context.Context) (BootstrapState, error) {
	s.clearReady()
	profile, configured, err := s.activeProfile(ctx)
	if err != nil {
		return BootstrapState{}, err
	}
	if !configured {
		return BootstrapState{State: StateUnconfigured}, nil
	}
	// Logout always revokes remotely (best effort) and removes the local
	// session; the profile and preferences survive (M1 contract).
	if s.vault != nil {
		if sessionID, ok := s.vault.Load(ctx, profile.ID); ok && sessionID != "" {
			_ = s.vault.Revoke(ctx, profile.BaseURL, sessionID)
		}
		_ = s.vault.Delete(ctx, profile.ID)
	}
	// A relay profile carries no vault session — its credential is the
	// relay-token preference row. Revoke it at the relay first (best
	// effort, same contract as vault.Revoke), then delete the local row;
	// otherwise the next Bootstrap silently re-opens with the
	// still-server-valid token.
	if token := s.store.ProfileRelayToken(ctx, profile.BaseURL); token != "" {
		_ = s.relayLogout(ctx, profile.BaseURL, token)
		if err := s.store.DeleteProfileRelayToken(ctx, profile.BaseURL); err != nil {
			return BootstrapState{}, err
		}
	}
	// The connect page's entry ① must survive logout — the embedded
	// bridge is still running and re-adoptable.
	return BootstrapState{State: StateAuthorizationNeeded, ServerURL: profile.BaseURL, EmbeddedURL: s.embeddedBaseURL()}, nil
}

// Session states for ProfileSummary (the connect page's badges).
const (
	ProfileSessionActive    = "active"
	ProfileSessionExpired   = "expired"
	ProfileSessionSignedOut = "signed_out"
)

// ProfileSummary is one saved connection on the connect page: where it
// points, whether its stored session can still open the app, and whether
// it is the profile Bootstrap would resume.
type ProfileSummary struct {
	ID           int64  `json:"id"`
	ServerURL    string `json:"server_url"`
	BridgeID     string `json:"bridge_id,omitempty"`
	SessionState string `json:"session_state"`
	LastUsedAt   int64  `json:"last_used_at"` // Unix seconds
	Active       bool   `json:"active"`
}

// ProfileDeletion reports what a DeleteProfile did: the local records are
// gone, and — when a live session existed — whether the server-side
// revocation succeeded (best effort, same contract as Logout).
type ProfileDeletion struct {
	Deleted    bool `json:"deleted"`
	HadSession bool `json:"had_session"`
	Revoked    bool `json:"revoked"`
}

// Profiles lists the saved connections for the connect page, live
// sessions first (one-tap resume), then most recently used.
func (s *Service) Profiles(ctx context.Context) ([]ProfileSummary, error) {
	if s.store == nil {
		return nil, errors.New("connection profile store is required")
	}
	rows, err := s.store.ListProfiles(ctx)
	if err != nil {
		return nil, err
	}
	nowMilli := s.now().UnixMilli()
	out := make([]ProfileSummary, 0, len(rows))
	for _, row := range rows {
		state := ProfileSessionSignedOut
		if row.HasSession {
			if row.SessionExpiresAt > nowMilli {
				state = ProfileSessionActive
			} else {
				state = ProfileSessionExpired
			}
		}
		out = append(out, ProfileSummary{
			ID:           row.ID,
			ServerURL:    row.BaseURL,
			BridgeID:     s.store.ProfileBridge(ctx, row.BaseURL),
			SessionState: state,
			LastUsedAt:   row.UpdatedAt,
			Active:       row.Active,
		})
	}
	// Live sessions first (the one-tap path), then recency.
	sort.SliceStable(out, func(i, j int) bool {
		if (out[i].SessionState == ProfileSessionActive) != (out[j].SessionState == ProfileSessionActive) {
			return out[i].SessionState == ProfileSessionActive
		}
		return out[i].LastUsedAt > out[j].LastUsedAt
	})
	return out, nil
}

// ResumeProfile makes the given profile active and re-enters it along its
// stored session — one tap back into a signed-in bridge. A profile
// without a live session still activates; the caller lands on the key
// form with the URL prefilled.
func (s *Service) ResumeProfile(ctx context.Context, id int64) (BootstrapState, error) {
	if s.store == nil {
		return BootstrapState{}, errors.New("connection profile store is required")
	}
	profile, ok, err := s.store.ActivateProfile(ctx, id)
	if err != nil {
		return BootstrapState{}, err
	}
	if !ok {
		return BootstrapState{}, errors.New("connection profile not found")
	}
	// A newly selected profile must not inherit readiness from the prior one.
	s.clearReady()
	state := BootstrapState{
		ServerURL:   profile.BaseURL,
		BridgeID:    s.store.ProfileBridge(ctx, profile.BaseURL),
		EmbeddedURL: s.embeddedBaseURL(),
	}
	if s.vault != nil {
		if sessionID, ok := s.vault.Load(ctx, profile.ID); ok && sessionID != "" {
			s.markReady(profile.BaseURL)
			state.State = StateReady
			return state, nil
		}
	}
	state.State = StateAuthorizationNeeded
	return state, nil
}

// DeleteProfile removes a saved connection and its local session. A live
// session is revoked at the server first (best effort — a failed
// revocation never blocks the local deletion, mirroring Logout).
func (s *Service) DeleteProfile(ctx context.Context, id int64) (ProfileDeletion, error) {
	if s.store == nil {
		return ProfileDeletion{}, errors.New("connection profile store is required")
	}
	profile, ok, err := s.store.ProfileByID(ctx, id)
	if err != nil {
		return ProfileDeletion{}, err
	}
	if !ok {
		return ProfileDeletion{}, errors.New("connection profile not found")
	}
	out := ProfileDeletion{}
	if s.vault != nil {
		if sessionID, live := s.vault.Load(ctx, profile.ID); live && sessionID != "" {
			out.HadSession = true
			out.Revoked = s.vault.Revoke(ctx, profile.BaseURL, sessionID) == nil
		}
		_ = s.vault.Delete(ctx, profile.ID)
	}
	if s.isReadyFor(profile.BaseURL) {
		s.clearReady()
	}
	out.Deleted, err = s.store.DeleteProfile(ctx, id)
	return out, err
}

func (s *Service) Preference(ctx context.Context, key string) (PreferenceValue, error) {
	if !knownPreferenceKey(key) {
		return PreferenceValue{}, errors.New("preference key is not allowed")
	}
	if s.store == nil {
		return PreferenceValue{}, errors.New("preference store is required")
	}
	value, found, err := s.store.Preference(ctx, key)
	if err != nil {
		return PreferenceValue{}, err
	}
	if found && !validPreferenceValue(key, value) {
		return PreferenceValue{}, errors.New("stored preference value is invalid")
	}
	return PreferenceValue{Value: value, Found: found}, nil
}

func (s *Service) SetPreference(ctx context.Context, input PreferenceInput) (PreferenceWriteResult, error) {
	if !knownPreferenceKey(input.Key) {
		return PreferenceWriteResult{}, errors.New("preference key is not allowed")
	}
	if !validPreferenceValue(input.Key, input.Value) {
		return PreferenceWriteResult{}, errors.New("preference value is invalid")
	}
	if input.Revision == 0 || input.Revision > 9_007_199_254_740_991 {
		return PreferenceWriteResult{}, errors.New("preference revision is invalid")
	}
	if s.store == nil {
		return PreferenceWriteResult{}, errors.New("preference store is required")
	}

	applied, err := s.store.PutPreferenceVersioned(ctx, input.Key, input.Value, input.Revision)
	if err != nil {
		return PreferenceWriteResult{Revision: input.Revision, Applied: false}, err
	}
	return PreferenceWriteResult{Revision: input.Revision, Applied: applied}, nil
}

func knownPreferenceKey(key string) bool {
	switch key {
	case "woca-theme",
		"woca.pref.locale",
		"woca.pref.autoCollapseTools",
		"woca.pref.markdownPreview",
		"woca.pref.model",
		"woca.pref.permissionMode":
		return true
	default:
		return false
	}
}

func validPreferenceValue(key, value string) bool {
	if len(value) > 256 || !utf8.ValidString(value) {
		return false
	}
	switch key {
	case "woca-theme":
		return value == "auto" || value == "dark" || value == "light"
	case "woca.pref.locale":
		return value == "auto" || value == "zh-CN" || value == "en"
	case "woca.pref.autoCollapseTools", "woca.pref.markdownPreview":
		return value == "true" || value == "false"
	case "woca.pref.model":
		if value == "" || strings.TrimSpace(value) != value || strings.Contains(value, "://") {
			return false
		}
		for _, r := range value {
			if unicode.IsControl(r) {
				return false
			}
		}
		return true
	case "woca.pref.permissionMode":
		switch value {
		case "default", "acceptEdits", "bypassPermissions", "readOnly", "plan":
			return true
		}
	}
	return false
}

// ApplyDevelopmentOverride enables the unauthenticated development transport
// only for a strict loopback HTTP origin. It deliberately has no API-key or
// credential parameter, so development configuration cannot reinterpret a
// bootstrap key as a session.
func (s *Service) ApplyDevelopmentOverride(ctx context.Context, rawURL string) (string, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil || !isLoopbackHTTPOrigin(parsed) {
		return "", errors.New("development server URL must be a loopback HTTP origin")
	}
	if s.store == nil {
		return "", errors.New("connection profile store is required")
	}
	profile, err := s.store.SaveProfile(ctx, rawURL)
	if err != nil {
		return "", err
	}
	s.markReady(profile.BaseURL)
	return profile.BaseURL, nil
}

func (s *Service) activeProfile(ctx context.Context) (configstore.Profile, bool, error) {
	if s.store == nil {
		return configstore.Profile{}, false, errors.New("connection profile store is required")
	}
	return s.store.ActiveProfile(ctx)
}

func (s *Service) isReadyFor(baseURL string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.readyBaseURL == baseURL
}

func (s *Service) markReady(baseURL string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.readyBaseURL = baseURL
}

func (s *Service) clearReady() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.readyBaseURL = ""
}

func isLoopbackHTTPOrigin(parsed *url.URL) bool {
	if parsed == nil || !strings.EqualFold(parsed.Scheme, "http") || parsed.Host == "" || parsed.User != nil {
		return false
	}
	if parsed.Path != "" && parsed.Path != "/" || parsed.RawPath != "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.RawFragment != "" {
		return false
	}
	host := parsed.Hostname()
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
