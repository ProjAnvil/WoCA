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
}

type App struct {
	ctx          context.Context
	bridge       bridgeAPI
	onShutdown   func()
	shutdownOnce sync.Once
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

func (a *App) context() context.Context {
	if a.ctx == nil {
		return context.Background()
	}
	return a.ctx
}
