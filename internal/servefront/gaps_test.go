package servefront

// Gap tests: listen failure, api-key auth without a settings database, and
// the web command's status path over a recorded (running) daemon record.

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestServeListenFailure(t *testing.T) {
	isolateEnv(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	opts := baseOpts(t, fakeOpenAI(t, "hi"))
	if err := Serve(t.Context(), opts, ln.Addr().String(), true, ""); err == nil {
		t.Fatal("Serve on a taken port should fail")
	}
}

// TestServeAPIKeyWithoutSettingsDB: with the settings database unavailable
// (blocked path), the api-key auth mode is rejected after the agent builds.
func TestServeAPIKeyWithoutSettingsDB(t *testing.T) {
	home := isolateEnv(t)
	blocker := filepath.Join(home, "blocker")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WOCA_DB", filepath.Join(blocker, "woca.db"))
	opts := baseOpts(t, fakeOpenAI(t, "hi"))
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	err := Serve(ctx, opts, "127.0.0.1:0", true, "apikey")
	if err == nil {
		t.Fatal("api-key auth without a settings DB should fail")
	}
}

// TestWebCmdStatusWithRunningRecord: a record naming a live pid (this test
// process) makes `woca web status` report the running daemon.
func TestWebCmdStatusWithRunningRecord(t *testing.T) {
	home := isolateEnv(t)
	rec := map[string]any{"pid": os.Getpid(), "addr": "127.0.0.1:3484", "started": "2026-08-21T00:00:00Z"}
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".woca"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".woca", "web.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := NewWebCmd()
	cmd.SetArgs([]string{"status"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("status: %v", err)
	}
}
