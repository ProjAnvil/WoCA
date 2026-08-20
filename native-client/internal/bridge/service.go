// Package bridge exposes the small native bootstrap surface that is bound to
// the Wails frontend. Authentication details remain behind auth.Activator.
package bridge

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/projanvil/woca/native-client/internal/auth"
	"github.com/projanvil/woca/native-client/internal/configstore"
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
}

type ConnectionInput struct {
	ServerURL string `json:"server_url"`
	APIKey    string `json:"api_key"`
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
	PutPreferenceVersioned(context.Context, string, string, uint64) (bool, error)
	Preference(context.Context, string) (string, bool, error)
}

// Service retains only the current process's ready marker. The future opaque,
// device-bound session is intentionally not represented here yet.
type Service struct {
	store     nativeStore
	activator auth.Activator

	mu           sync.RWMutex
	readyBaseURL string
}

func NewService(store nativeStore, activator auth.Activator) *Service {
	if activator == nil {
		activator = auth.UnavailableActivator{}
	}
	return &Service{store: store, activator: activator}
}

func (s *Service) Bootstrap(ctx context.Context) (BootstrapState, error) {
	profile, configured, err := s.activeProfile(ctx)
	if err != nil {
		return BootstrapState{}, err
	}
	if !configured {
		return BootstrapState{State: StateUnconfigured}, nil
	}
	if s.isReadyFor(profile.BaseURL) {
		return BootstrapState{State: StateReady, ServerURL: profile.BaseURL}, nil
	}
	return BootstrapState{State: StateAuthorizationNeeded, ServerURL: profile.BaseURL}, nil
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

	result, err := s.activator.Activate(ctx, auth.ActivationRequest{
		BaseURL: profile.BaseURL,
		APIKey:  input.APIKey,
	})
	if err != nil {
		return BootstrapState{State: StateAuthorizationNeeded, ServerURL: profile.BaseURL}, err
	}
	if result.Ready {
		s.markReady(profile.BaseURL)
		return BootstrapState{State: StateReady, ServerURL: profile.BaseURL}, nil
	}
	return BootstrapState{State: StateAuthorizationNeeded, ServerURL: profile.BaseURL}, nil
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
	return BootstrapState{State: StateAuthorizationNeeded, ServerURL: profile.BaseURL}, nil
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
