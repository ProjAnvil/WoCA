package relay

import (
	"encoding/json"
	"net/http"
)

// Wire error codes (design §2/§9). "revoked" is a frontend contract — the
// SPA and native clients program against exactly this string (never
// "key_revoked"); internal/platform/authn.CodeRevoked is the personal
// binary's own copy of the same vocabulary.
const (
	CodeUnauthorized  = "unauthorized"
	CodeRevoked       = "revoked"
	CodeBridgeOffline = "bridge_offline"
)

// WriteError emits the relay's JSON error envelope.
func WriteError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"code": code, "error": message})
}

// writeJSON is the success-path counterpart.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
