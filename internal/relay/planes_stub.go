package relay

// Placeholder declarations for the forwarding/revocation planes so the
// Task 3 serve skeleton compiles with its final Relay shape. Tasks 5-6
// replace these stubs with the real implementations (delete this file
// when the real types land).

// TunnelService is the authenticated forwarding plane (/tunnel/ routes).
// Defined fully by the tunnel task.
type TunnelService struct{}

// AuditLog records key/session lifecycle events. Defined fully by the
// revocation task.
type AuditLog struct{}
