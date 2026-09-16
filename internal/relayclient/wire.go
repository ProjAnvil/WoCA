// Package relayclient is the bridge side of the personal remote relay
// (design 2026-09-17-personal-relay-design §3.2): an outbound long-poll
// tunnel client compiled into the personal woca binary. It executes
// parked client requests against the in-process server handler — the same
// code path as local web traffic, no TCP loopback — and streams answers
// back chunk by chunk.
//
// The wire vocabulary below mirrors internal/relay deliberately (the two
// packages stay independently compilable; semantics copied from the
// enterprise internal/ee/bridge tunnel, code rewritten — nothing is
// imported from internal/ee).
package relayclient

// KeyPrefix is relayclient's own copy of the relay key prefix (used by
// the CLI to validate input without importing the relay server package).
const KeyPrefix = "wcr_"

// Tunnel wire headers (mirror internal/relay).
const (
	generationHeader = "X-Woca-Relay-Generation"
	statusHeader     = "X-Woca-Relay-Status"
	headersHeader    = "X-Woca-Relay-Headers"
	remoteHeader     = "X-Woca-Relay-Remote"
)

// pollPayload mirrors the relay's parked-request wire shape (the 200 body
// of /tunnel/poll).
type pollPayload struct {
	ID      string              `json:"id"`
	Method  string              `json:"method"`
	Path    string              `json:"path"`
	Query   string              `json:"query"`
	Client  string              `json:"client"`
	Headers map[string][]string `json:"headers"`
	Body    string              `json:"body"` // base64 standard encoding
}
