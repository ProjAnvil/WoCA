package worker

import (
	"fmt"
	"os"

	"github.com/projanvil/woca/internal/wocadb"
)

// NewDBSessionStates wires SessionStates over the shared settings database
// (the same session_state rows the gateways' listings read).
func NewDBSessionStates(db *wocadb.DB) SessionStates {
	return &dbSessionStates{db: db}
}

// dbSessionStates implements SessionStates over the shared session_state
// table (the same rows the gateways' listings read).
type dbSessionStates struct {
	db *wocadb.DB
}

func (s *dbSessionStates) MarkRunning(sessionID, runID string) {
	_ = s.db.SetSessionState(wocadb.SessionState{
		SessionID: sessionID,
		Status:    wocadb.SessionRunning,
		RunID:     runID,
		Owner:     fmt.Sprintf("%d@worker", os.Getpid()),
		UpdatedAt: wocadb.Now(),
	})
}

func (s *dbSessionStates) Clear(sessionID, runID string) {
	_, _ = s.db.ClearSessionState(sessionID, runID)
}
