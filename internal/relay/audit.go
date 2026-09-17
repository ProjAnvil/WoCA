package relay

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// Audit event kinds (design §5.4: login failures, revocations, bridge
// up/down).
const (
	AuditLoginFailed = "login_failed"
	AuditLoginOK     = "login_ok"
	AuditRevocation  = "revocation"
	AuditBridgeUp    = "bridge_up"
	AuditBridgeDown  = "bridge_down"
)

// AuditLog is the relay's append-only local audit trail (design §5.4):
// one JSON object per line. It records metadata only — never key
// material, never session tokens.
type AuditLog struct {
	mu sync.Mutex
	w  io.Writer
	c  io.Closer
}

// NewAuditLog opens path for appending (0600). An empty path discards
// entries — tests and ephemeral deployments stay quiet.
func NewAuditLog(path string) (*AuditLog, error) {
	if path == "" {
		return &AuditLog{w: io.Discard}, nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("relay: audit log: %w", err)
	}
	return &AuditLog{w: f, c: f}, nil
}

// Event appends one audit line: {"ts","event","key","detail"}.
func (a *AuditLog) Event(kind, keyName, detail string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	_ = json.NewEncoder(a.w).Encode(map[string]any{
		"ts":     time.Now().UTC().Format(time.RFC3339),
		"event":  kind,
		"key":    keyName,
		"detail": detail,
	})
}

// Close flushes and closes the underlying file (nil-safe for the
// discard writer).
func (a *AuditLog) Close() error {
	if a.c != nil {
		return a.c.Close()
	}
	return nil
}
