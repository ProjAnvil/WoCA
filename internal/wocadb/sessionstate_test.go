package wocadb

import (
	"path/filepath"
	"testing"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open("sqlite:" + filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestSessionStateLifecycle(t *testing.T) {
	db := openTestDB(t)

	st := SessionState{
		SessionID: "sess-1", Status: SessionRunning,
		RunID: "run-1", Owner: "111@h", UpdatedAt: Now(),
	}
	if err := db.SetSessionState(st); err != nil {
		t.Fatal(err)
	}
	// Upsert flips status and run id together.
	st.Status = SessionAwaiting
	st.RunID = "run-2"
	if err := db.SetSessionState(st); err != nil {
		t.Fatal(err)
	}

	rows, err := db.SessionStates()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Status != SessionAwaiting || rows[0].RunID != "run-2" || rows[0].Owner != "111@h" {
		t.Fatalf("rows = %+v", rows)
	}

	// Compare-and-clear: a stale run id must not remove the promoted run's row.
	if ok, err := db.ClearSessionState("sess-1", "run-1"); err != nil || ok {
		t.Fatalf("stale clear = %v, %v; want false, nil", ok, err)
	}
	if ok, err := db.ClearSessionState("sess-1", "run-2"); err != nil || !ok {
		t.Fatalf("matching clear = %v, %v; want true, nil", ok, err)
	}
	rows, err = db.SessionStates()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("after clear rows = %+v", rows)
	}

	// Unconditional delete is idempotent.
	if err := db.SetSessionState(st); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteSessionState("sess-1"); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteSessionState("sess-1"); err != nil {
		t.Fatal(err)
	}
}
