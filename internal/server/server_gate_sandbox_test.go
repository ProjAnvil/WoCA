package server

// The dynamic RunsOffHost gate (design §4.5 / §10.2 M-3): the run surface
// stays open to remote tenants only while a sandbox backend is enabled,
// and a queued run is admitted only while a live sandbox-capable worker
// acks. Otherwise: 403 (no sandbox) / 503 (no capable worker).

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/projanvil/woca/internal/runstate"
	"github.com/projanvil/woca/internal/wocadb"
	"github.com/rs/zerolog"
)

func newGateServer(t *testing.T, runsOffHost bool, sandboxEnabled func() bool) (*Server, *wocadb.DB) {
	t.Helper()
	db, err := wocadb.Open("sqlite:" + filepath.Join(t.TempDir(), "gate.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ledger, err := runstate.New(db)
	if err != nil {
		t.Fatal(err)
	}
	var se func() bool
	if sandboxEnabled != nil {
		se = sandboxEnabled
	}
	s := New(Config{
		Agent: newScriptedAgent(t, "x"), Static: nil, Logger: zerolog.Nop(),
		SessionDir: t.TempDir(), WorkDir: t.TempDir(),
		RunLedger: ledger, DispatchToQueue: true, RunsOffHost: runsOffHost,
		RunStateDB:     db,
		SandboxEnabled: se,
		Auth:           tenantProvider{tenant: "tenant-remote"},
	})
	return s, db
}

// ackWorker writes one worker ack row with the given age and capabilities.
func ackWorker(t *testing.T, db *wocadb.DB, ageMs int64, capabilities string) {
	t.Helper()
	if err := db.SetConfigAck(wocadb.ConfigAck{
		ServiceID: "wkr-1", Role: "worker", Generation: "g1",
		Capabilities: capabilities, UpdatedAt: wocadb.Now() - ageMs,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSandboxGateMatrix(t *testing.T) {
	cases := []struct {
		name           string
		offHost        bool
		sandboxEnabled func() bool
		ackAgeMs       int64 // -1 = no ack row
		ackCaps        string
		want           int
	}{
		{"no offHost stays local-gated", false, nil, -1, "", http.StatusForbidden},
		{"offHost without sandbox is fail-closed", true, nil, -1, "", http.StatusForbidden},
		{"offHost with disabled sandbox is fail-closed", true, func() bool { return false }, -1, "", http.StatusForbidden},
		{"sandbox but no live worker yields 503", true, func() bool { return true }, -1, "", http.StatusServiceUnavailable},
		{"sandbox but stale worker ack yields 503", true, func() bool { return true }, 30_000, "sandbox", http.StatusServiceUnavailable},
		{"sandbox with live capable worker admits", true, func() bool { return true }, 5_000, "sandbox", http.StatusOK},
		{"live worker without sandbox capability yields 503", true, func() bool { return true }, 5_000, "", http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, db := newGateServer(t, tc.offHost, tc.sandboxEnabled)
			if tc.ackAgeMs >= 0 {
				ackWorker(t, db, tc.ackAgeMs, tc.ackCaps)
			}
			srv := httptest.NewServer(s.Handler())
			defer srv.Close()

			resp := dispatchPost(t, srv.URL, "s-gate", "hello")
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("POST /api/runs = %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
}
