package main

import (
	"errors"
	"testing"

	"github.com/projanvil/woca/native-client/internal/bridge"
)

func TestFatalLocalDataBridgeBootstrapReportsOnlyFixedStateAndPath(t *testing.T) {
	service := newFatalLocalDataBridge("/Users/test/Library/Application Support/woca-native-client/client.db")

	state, err := service.Bootstrap(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := bridge.BootstrapState{
		State:        bridge.StateFatalLocalData,
		DatabasePath: "/Users/test/Library/Application Support/woca-native-client/client.db",
	}
	if state != want {
		t.Fatalf("Bootstrap() = %#v, want %#v", state, want)
	}
}

func TestFatalLocalDataBridgeRejectsEveryMutatingOrStorageOperation(t *testing.T) {
	service := newFatalLocalDataBridge("/tmp/client.db")
	wantState := bridge.BootstrapState{State: bridge.StateFatalLocalData, DatabasePath: "/tmp/client.db"}

	connectState, connectErr := service.Connect(t.Context(), bridge.ConnectionInput{
		ServerURL: "https://enterprise.example.com",
		APIKey:    "must-not-be-used",
	})
	logoutState, logoutErr := service.Logout(t.Context())
	preference, preferenceErr := service.Preference(t.Context(), "woca-theme")
	write, writeErr := service.SetPreference(t.Context(), bridge.PreferenceInput{
		Key: "woca-theme", Value: "dark", Revision: 1,
	})

	if connectState != wantState || logoutState != wantState {
		t.Fatalf("fatal states = connect %#v, logout %#v", connectState, logoutState)
	}
	for name, err := range map[string]error{
		"Connect": connectErr, "Logout": logoutErr,
		"Preference": preferenceErr, "SetPreference": writeErr,
	} {
		if !errors.Is(err, errFatalLocalData) {
			t.Fatalf("%s error = %v, want fatal local-data error", name, err)
		}
	}
	if preference != (bridge.PreferenceValue{}) || write != (bridge.PreferenceWriteResult{}) {
		t.Fatalf("storage results = %#v, %#v; want zero values", preference, write)
	}
}
