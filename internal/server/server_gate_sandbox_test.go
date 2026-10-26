package server

// The dynamic RunsOffHost gate (design §3.5 / §4.5 / §10.2 M-3): the run
// surface stays open to remote tenants only while a sandbox backend is
// enabled, and a queued run is admitted only while a live sandbox-capable
// executor acks (the worker role no longer qualifies — the EE execution
// plane is the executor). Otherwise: 403 (no sandbox) / 503 (no capable
// executor).

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

// ackService writes one ack row with the given role, age and capabilities.
func ackService(t *testing.T, db *wocadb.DB, role string, ageMs int64, capabilities string) {
	t.Helper()
	if err := db.SetConfigAck(wocadb.ConfigAck{
		ServiceID: role + "-1", Role: role, Generation: "g1",
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
		ackRole        string // "" = no ack row
		ackAgeMs       int64
		ackCaps        string
		want           int
	}{
		{"no offHost stays local-gated", false, nil, "", -1, "", http.StatusForbidden},
		{"offHost without sandbox is fail-closed", true, nil, "", -1, "", http.StatusForbidden},
		{"offHost with disabled sandbox is fail-closed", true, func() bool { return false }, "", -1, "", http.StatusForbidden},
		{"sandbox but no executor ack yields 503", true, func() bool { return true }, "", -1, "", http.StatusServiceUnavailable},
		{"stale executor ack yields 503", true, func() bool { return true }, "executor", 30_000, "sandbox", http.StatusServiceUnavailable},
		{"live executor with sandbox capability admits", true, func() bool { return true }, "executor", 5_000, "sandbox", http.StatusOK},
		{"live executor without sandbox capability yields 503", true, func() bool { return true }, "executor", 5_000, "", http.StatusServiceUnavailable},
		// The worker role no longer satisfies the gate: only executor acks
		// count (spec §3.5), even when the worker advertises sandbox.
		{"live worker with sandbox capability yields 503", true, func() bool { return true }, "worker", 5_000, "sandbox", http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, db := newGateServer(t, tc.offHost, tc.sandboxEnabled)
			if tc.ackRole != "" {
				ackService(t, db, tc.ackRole, tc.ackAgeMs, tc.ackCaps)
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

// TestSandboxGateAckReadFailure: a config_acks read failure must fail
// closed — an off-host run is never queued on an unknown executor state.
func TestSandboxGateAckReadFailure(t *testing.T) {
	s, db := newGateServer(t, true, func() bool { return true })
	ackService(t, db, "executor", 5_000, "sandbox")
	if err := db.Close(); err != nil { // the ack read now fails
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp := dispatchPost(t, srv.URL, "s-gate-readfail", "hello")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("POST /api/runs = %d, want %d", resp.StatusCode, http.StatusServiceUnavailable)
	}
}
