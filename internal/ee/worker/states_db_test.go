package worker

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/projanvil/woca/internal/wocadb"
)

func TestDBSessionStatesMarkClear(t *testing.T) {
	db, err := wocadb.Open("sqlite:" + filepath.Join(t.TempDir(), "states.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	st := NewDBSessionStates(db)
	if st == nil {
		t.Fatal("NewDBSessionStates = nil, want a states store")
	}

	// MarkRunning upserts the running row the gateways' listings read.
	st.MarkRunning("sess-1", "run-1")
	rows, err := db.SessionStates()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("session_state rows = %d, want 1", len(rows))
	}
	row := rows[0]
	if row.SessionID != "sess-1" || row.RunID != "run-1" || row.Status != wocadb.SessionRunning {
		t.Fatalf("row = %+v, want sess-1/run-1/running", row)
	}
	if want := fmt.Sprintf("%d@worker", os.Getpid()); row.Owner != want {
		t.Errorf("owner = %q, want %q", row.Owner, want)
	}

	// Clear is compare-and-clear: a mismatched run ID leaves the row alone
	// (a promoted run's row stays safe).
	st.Clear("sess-1", "run-other")
	if rows, err = db.SessionStates(); err != nil || len(rows) != 1 {
		t.Fatalf("rows after clear(wrong run) = %v (err %v), want 1", rows, err)
	}

	st.Clear("sess-1", "run-1")
	if rows, err = db.SessionStates(); err != nil || len(rows) != 0 {
		t.Fatalf("rows after clear = %v (err %v), want none", rows, err)
	}
}
