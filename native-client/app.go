package main

import (
	"context"
	"errors"

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
	ctx    context.Context
	bridge bridgeAPI
}

func NewApp(service bridgeAPI) *App {
	return &App{bridge: service}
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
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
