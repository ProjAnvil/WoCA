package worker

import (
	"context"
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
// table (the same rows the gateways' listings read). The context scopes
// each write to the claimed run's tenant — execute restores it from the run
// row before these calls fire — so the busy rows this worker writes land
// under the owning tenant (the table is FORCE RLS-scoped on postgres) and
// that tenant's gateway listings see them.
type dbSessionStates struct {
	db *wocadb.DB
}

func (s *dbSessionStates) MarkRunning(ctx context.Context, sessionID, runID string) {
	_ = s.db.SetSessionState(ctx, wocadb.SessionState{
		SessionID: sessionID,
		Status:    wocadb.SessionRunning,
		RunID:     runID,
		Owner:     fmt.Sprintf("%d@worker", os.Getpid()),
		UpdatedAt: wocadb.Now(),
	})
}

func (s *dbSessionStates) Clear(ctx context.Context, sessionID, runID string) {
	_, _ = s.db.ClearSessionState(ctx, sessionID, runID)
}
