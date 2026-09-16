package main

import (
	"context"
	"errors"

	"github.com/projanvil/woca/native-client/internal/bridge"
)

var errFatalLocalData = errors.New("local data is unavailable")

type fatalLocalDataBridge struct {
	state bridge.BootstrapState
}

func newFatalLocalDataBridge(databasePath string) *fatalLocalDataBridge {
	return &fatalLocalDataBridge{state: bridge.BootstrapState{
		State:        bridge.StateFatalLocalData,
		DatabasePath: databasePath,
	}}
}

func (b *fatalLocalDataBridge) Bootstrap(context.Context) (bridge.BootstrapState, error) {
	return b.state, nil
}

func (b *fatalLocalDataBridge) Connect(context.Context, bridge.ConnectionInput) (bridge.BootstrapState, error) {
	return b.state, errFatalLocalData
}

func (b *fatalLocalDataBridge) ConnectRelay(context.Context, bridge.ConnectionInput) (bridge.BootstrapState, error) {
	return b.state, errFatalLocalData
}

func (b *fatalLocalDataBridge) Logout(context.Context) (bridge.BootstrapState, error) {
	return b.state, errFatalLocalData
}

func (b *fatalLocalDataBridge) ConnectEmbedded(context.Context) (bridge.BootstrapState, error) {
	return b.state, errFatalLocalData
}

func (b *fatalLocalDataBridge) Profiles(context.Context) ([]bridge.ProfileSummary, error) {
	return nil, errFatalLocalData
}

func (b *fatalLocalDataBridge) ResumeProfile(context.Context, int64) (bridge.BootstrapState, error) {
	return b.state, errFatalLocalData
}

func (b *fatalLocalDataBridge) DeleteProfile(context.Context, int64) (bridge.ProfileDeletion, error) {
	return bridge.ProfileDeletion{}, errFatalLocalData
}

func (b *fatalLocalDataBridge) Preference(context.Context, string) (bridge.PreferenceValue, error) {
	return bridge.PreferenceValue{}, errFatalLocalData
}

func (b *fatalLocalDataBridge) SetPreference(context.Context, bridge.PreferenceInput) (bridge.PreferenceWriteResult, error) {
	return bridge.PreferenceWriteResult{}, errFatalLocalData
}
