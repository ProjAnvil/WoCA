package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/projanvil/woca/native-client/internal/bridge"
	"github.com/projanvil/woca/native-client/internal/configstore"
	"github.com/projanvil/woca/native-client/internal/transport"
)

// bridgeAPI is deliberately the entire Wails-facing backend surface. Storage,
// credentials, and remote requests belong to the bridge and its dependencies.
type bridgeAPI interface {
	Bootstrap(context.Context) (bridge.BootstrapState, error)
	Connect(context.Context, bridge.ConnectionInput) (bridge.BootstrapState, error)
	Logout(context.Context) (bridge.BootstrapState, error)
	ConnectEmbedded(context.Context) (bridge.BootstrapState, error)
	Preference(context.Context, string) (bridge.PreferenceValue, error)
	SetPreference(context.Context, bridge.PreferenceInput) (bridge.PreferenceWriteResult, error)
}

type requestCancellationRegistry interface {
	Cancel(string) bool
	CancelAll()
}

// StreamCredential is the auth header material for direct streaming
// requests (SSE bypasses the asset proxy; the WebView cannot reach the
// in-process credential chain any other way).
type StreamCredential struct {
	Scheme string `json:"scheme"`
	Token  string `json:"token"`
}

// streamCredentialSource supplies credentials for a target URL (the
// transport's provider; nil disables the bound method).
type streamCredentialSource interface {
	Credentials(ctx context.Context, method string, target *url.URL) (transport.Credentials, error)
}

// keySink persists a bridge API key per server URL (the OS keychain
// store; product design §2.2: keys live in the credential vault so
// session expiry re-authenticates silently). Receives the normalized
// base URL.
type keySink func(baseURL, apiKey string)

type App struct {
	ctx           context.Context
	bridge        bridgeAPI
	cancellations requestCancellationRegistry
	streamAuth    streamCredentialSource
	storeKey      keySink
	onShutdown    func()
	shutdownOnce  sync.Once

	preferenceMu      sync.Mutex
	preferenceCalls   sync.WaitGroup
	preferenceClosing bool
}

func NewApp(service bridgeAPI) *App {
	return newAppWithShutdown(service, nil)
}

func newAppWithShutdown(service bridgeAPI, onShutdown func()) *App {
	return newAppWithCancellation(service, nil, onShutdown)
}

func newAppWithCancellation(
	service bridgeAPI,
	cancellations requestCancellationRegistry,
	onShutdown func(),
) *App {
	return &App{bridge: service, cancellations: cancellations, onShutdown: onShutdown}
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
}

func (a *App) shutdown(context.Context) {
	a.shutdownOnce.Do(func() {
		a.preferenceMu.Lock()
		a.preferenceClosing = true
		a.preferenceMu.Unlock()
		if a.cancellations != nil {
			a.cancellations.CancelAll()
		}
		a.preferenceCalls.Wait()
		if a.onShutdown != nil {
			a.onShutdown()
		}
	})
}

// CancelRequest bridges AbortController to the request context created by the
// native AssetServer handler. The opaque ID has no authority beyond this
// process-local registry.
func (a *App) CancelRequest(id string) bool {
	if a.cancellations == nil {
		return false
	}
	return a.cancellations.Cancel(id)
}

func (a *App) Bootstrap() (bridge.BootstrapState, error) {
	if a.bridge == nil {
		return bridge.BootstrapState{}, errors.New("native bridge is required")
	}
	return a.bridge.Bootstrap(a.context())
}

func (a *App) Connect(input bridge.ConnectionInput) (bridge.BootstrapState, error) {
	if a.bridge == nil {
		return bridge.BootstrapState{}, errors.New("native bridge is required")
	}
	state, err := a.bridge.Connect(a.context(), input)
	// Persist the key on success (§2.2: the credential vault is the only
	// client-side key storage). Failures stay silent — a full keychain is
	// an convenience, never a precondition for connecting.
	if err == nil && state.State == bridge.StateReady && a.storeKey != nil && input.APIKey != "" {
		if baseURL, nerr := configstore.NormalizeBaseURL(input.ServerURL); nerr == nil {
			a.storeKey(baseURL, input.APIKey)
		}
	}
	return state, err
}

func (a *App) Logout() (bridge.BootstrapState, error) {
	if a.bridge == nil {
		return bridge.BootstrapState{}, errors.New("native bridge is required")
	}
	return a.bridge.Logout(a.context())
}

// ConnectEmbedded is connection-page entry ① (design §6.6): adopt the
// embedded bridge — auto-launched, loopback-trusted, zero inputs.
func (a *App) ConnectEmbedded() (bridge.BootstrapState, error) {
	if a.bridge == nil {
		return bridge.BootstrapState{}, errors.New("native bridge is required")
	}
	return a.bridge.ConnectEmbedded(a.context())
}

// EdgeBridge is one bridge behind an edge access point.
type EdgeBridge struct {
	BridgeID  string `json:"bridge_id"`
	CreatedAt int64  `json:"created_at"`
	LastSeen  int64  `json:"last_seen"`
	Live      bool   `json:"live"`
}

// StreamCredentials hands the WebView the session material for direct
// streaming requests: the run-events SSE connection bypasses the asset
// proxy (WebView2 buffers proxied streams), so it needs the same Bearer
// the proxy would inject. Loopback targets need nothing; failures return
// empty material and the stream authenticates the way it used to.
func (a *App) StreamCredentials(serverURL string) (StreamCredential, error) {
	if a.streamAuth == nil {
		return StreamCredential{}, nil
	}
	base := strings.TrimRight(strings.TrimSpace(serverURL), "/")
	parsed, err := url.Parse(base)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return StreamCredential{}, errors.New("a valid server URL is required")
	}
	if host := parsed.Hostname(); host == "localhost" || (net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback()) {
		return StreamCredential{}, nil
	}
	ctx, cancel := context.WithTimeout(a.context(), 15*time.Second)
	defer cancel()
	creds, err := a.streamAuth.Credentials(ctx, http.MethodGet, parsed)
	if err != nil || creds.Session == "" {
		return StreamCredential{}, nil
	}
	return StreamCredential{Scheme: creds.Scheme, Token: creds.Session}, nil
}

// EdgeBridges lists the bridges registered at an edge (connection page
// entry ② target selection, design §6.6). Pre-auth by design: the choice
// happens before login; the edge's own inventory route answers.
func (a *App) EdgeBridges(serverURL string) ([]EdgeBridge, error) {
	base := strings.TrimRight(strings.TrimSpace(serverURL), "/")
	if parsed, err := url.Parse(base); err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, errors.New("a valid edge URL is required")
	}
	ctx, cancel := context.WithTimeout(a.context(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/edge/bridges", nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New("edge returned " + resp.Status)
	}
	var bridges []EdgeBridge
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&bridges); err != nil {
		return nil, err
	}
	return bridges, nil
}

func (a *App) Preference(key string) (bridge.PreferenceValue, error) {
	if a.bridge == nil {
		return bridge.PreferenceValue{}, errors.New("native bridge is required")
	}
	if err := a.beginPreferenceCall(); err != nil {
		return bridge.PreferenceValue{}, err
	}
	defer a.preferenceCalls.Done()
	return a.bridge.Preference(a.context(), key)
}

func (a *App) SetPreference(input bridge.PreferenceInput) (bridge.PreferenceWriteResult, error) {
	if a.bridge == nil {
		return bridge.PreferenceWriteResult{}, errors.New("native bridge is required")
	}
	if err := a.beginPreferenceCall(); err != nil {
		return bridge.PreferenceWriteResult{}, err
	}
	defer a.preferenceCalls.Done()
	return a.bridge.SetPreference(a.context(), input)
}

func (a *App) beginPreferenceCall() error {
	a.preferenceMu.Lock()
	defer a.preferenceMu.Unlock()
	if a.preferenceClosing {
		return errors.New("native application is shutting down")
	}
	a.preferenceCalls.Add(1)
	return nil
}

func (a *App) context() context.Context {
	if a.ctx == nil {
		return context.Background()
	}
	return a.ctx
}
