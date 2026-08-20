package main

import (
	"context"
	"errors"
	"sync"

	"github.com/projanvil/woca/native-client/internal/bridge"
)

// bridgeAPI is deliberately the entire Wails-facing backend surface. Storage,
// credentials, and remote requests belong to the bridge and its dependencies.
type bridgeAPI interface {
	Bootstrap(context.Context) (bridge.BootstrapState, error)
	Connect(context.Context, bridge.ConnectionInput) (bridge.BootstrapState, error)
	Logout(context.Context) (bridge.BootstrapState, error)
	Preference(context.Context, string) (bridge.PreferenceValue, error)
	SetPreference(context.Context, bridge.PreferenceInput) (bridge.PreferenceWriteResult, error)
}

type App struct {
	ctx          context.Context
	bridge       bridgeAPI
	onShutdown   func()
	shutdownOnce sync.Once

	preferenceMu      sync.Mutex
	preferenceCalls   sync.WaitGroup
	preferenceClosing bool
}

func NewApp(service bridgeAPI) *App {
	return newAppWithShutdown(service, nil)
}

func newAppWithShutdown(service bridgeAPI, onShutdown func()) *App {
	return &App{bridge: service, onShutdown: onShutdown}
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
}

func (a *App) shutdown(context.Context) {
	a.shutdownOnce.Do(func() {
		a.preferenceMu.Lock()
		a.preferenceClosing = true
		a.preferenceMu.Unlock()
		a.preferenceCalls.Wait()
		if a.onShutdown != nil {
			a.onShutdown()
		}
	})
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
	return a.bridge.Connect(a.context(), input)
}

func (a *App) Logout() (bridge.BootstrapState, error) {
	if a.bridge == nil {
		return bridge.BootstrapState{}, errors.New("native bridge is required")
	}
	return a.bridge.Logout(a.context())
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
