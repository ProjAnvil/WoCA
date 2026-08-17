package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoginFailures(t *testing.T) {
	isolateEnv(t)

	// Device endpoint down: BeginDeviceFlow fails.
	t.Setenv("WOCA_OAUTH_CLIENT_ID", "c")
	t.Setenv("WOCA_OAUTH_DEVICE_ENDPOINT", "http://127.0.0.1:1/device")
	t.Setenv("WOCA_OAUTH_TOKEN_ENDPOINT", "http://127.0.0.1:1/token")
	if err := login(t.Context()); err == nil {
		t.Fatal("expected device-flow error")
	}
}

func TestLoginTokenRejected(t *testing.T) {
	isolateEnv(t)
	// The device endpoint works; the token endpoint rejects with a
	// non-pending error, failing the poll quickly.
	mux := http_Mux(t,
		"/device", `{"device_code":"dc","user_code":"UC","verification_uri":"https://example.com/d","interval":0,"expires_in":60}`,
		"/token", `{"error":"invalid_grant"}`,
	)
	t.Setenv("WOCA_OAUTH_CLIENT_ID", "c")
	t.Setenv("WOCA_OAUTH_DEVICE_ENDPOINT", mux+"/device")
	t.Setenv("WOCA_OAUTH_TOKEN_ENDPOINT", mux+"/token")
	if err := login(t.Context()); err == nil {
		t.Fatal("expected token error")
	}
}

func TestLoginMkdirFailure(t *testing.T) {
	// HOME points at a regular file: credentials dir creation fails after a
	// successful device flow.
	home := t.TempDir()
	blocker := filepath.Join(home, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	mux := http_Mux(t,
		"/device", `{"device_code":"dc","user_code":"UC","verification_uri":"https://example.com/d","interval":0,"expires_in":60}`,
		"/token", `{"access_token":"at","refresh_token":"rt"}`,
	)
	t.Setenv("HOME", blocker)
	t.Setenv("WOCA_OAUTH_CLIENT_ID", "c")
	t.Setenv("WOCA_OAUTH_DEVICE_ENDPOINT", mux+"/device")
	t.Setenv("WOCA_OAUTH_TOKEN_ENDPOINT", mux+"/token")
	if err := login(t.Context()); err == nil {
		t.Fatal("expected mkdir failure")
	}
}

func TestLoginWriteFailure(t *testing.T) {
	home := t.TempDir()
	// credentials.json exists as a directory: the final write fails.
	if err := os.MkdirAll(filepath.Join(home, ".woca", "credentials.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	mux := http_Mux(t,
		"/device", `{"device_code":"dc","user_code":"UC","verification_uri":"https://example.com/d","interval":0,"expires_in":60}`,
		"/token", `{"access_token":"at","refresh_token":"rt"}`,
	)
	t.Setenv("HOME", home)
	t.Setenv("WOCA_OAUTH_CLIENT_ID", "c")
	t.Setenv("WOCA_OAUTH_DEVICE_ENDPOINT", mux+"/device")
	t.Setenv("WOCA_OAUTH_TOKEN_ENDPOINT", mux+"/token")
	if err := login(t.Context()); err == nil {
		t.Fatal("expected write failure")
	}
}
