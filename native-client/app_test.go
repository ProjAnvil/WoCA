package main

import (
	"context"
	"testing"

	"github.com/projanvil/woca/native-client/internal/bridge"
)

type fakeBridgeAPI struct {
	bootstrapState bridge.BootstrapState
	connectState   bridge.BootstrapState
	logoutState    bridge.BootstrapState

	bootstrapCtx context.Context
	connectCtx   context.Context
	logoutCtx    context.Context
	connectInput bridge.ConnectionInput
}

func (f *fakeBridgeAPI) Bootstrap(ctx context.Context) (bridge.BootstrapState, error) {
	f.bootstrapCtx = ctx
	return f.bootstrapState, nil
}

func (f *fakeBridgeAPI) Connect(ctx context.Context, input bridge.ConnectionInput) (bridge.BootstrapState, error) {
	f.connectCtx = ctx
	f.connectInput = input
	return f.connectState, nil
}

func (f *fakeBridgeAPI) Logout(ctx context.Context) (bridge.BootstrapState, error) {
	f.logoutCtx = ctx
	return f.logoutState, nil
}

func TestAppDelegatesWithStartupContext(t *testing.T) {
	service := &fakeBridgeAPI{
		bootstrapState: bridge.BootstrapState{State: bridge.StateUnconfigured},
		connectState:   bridge.BootstrapState{State: bridge.StateReady, ServerURL: "https://enterprise.example.com"},
		logoutState:    bridge.BootstrapState{State: bridge.StateAuthorizationNeeded, ServerURL: "https://enterprise.example.com"},
	}
	app := NewApp(service)
	startupCtx := context.WithValue(context.Background(), "source", "wails")
	app.startup(startupCtx)

	if got, err := app.Bootstrap(); err != nil || got != service.bootstrapState {
		t.Fatalf("Bootstrap() = %#v, %v", got, err)
	}
	input := bridge.ConnectionInput{ServerURL: "https://enterprise.example.com", APIKey: "ephemeral-key"}
	if got, err := app.Connect(input); err != nil || got != service.connectState {
		t.Fatalf("Connect() = %#v, %v", got, err)
	}
	if got, err := app.Logout(); err != nil || got != service.logoutState {
		t.Fatalf("Logout() = %#v, %v", got, err)
	}
	if service.bootstrapCtx != startupCtx || service.connectCtx != startupCtx || service.logoutCtx != startupCtx {
		t.Fatal("App did not delegate using its startup context")
	}
	if service.connectInput != input {
		t.Fatalf("Connect input = %#v, want %#v", service.connectInput, input)
	}
}
