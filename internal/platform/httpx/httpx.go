// Package httpx holds the shared JSON-envelope helpers every HTTP surface
// uses. One implementation instead of per-package copies that had already
// drifted (charset presence, error-body shapes).
package httpx

import (
	"encoding/json"
	"net/http"
)

// WriteJSON emits v as a UTF-8 JSON body. charset=utf-8 is explicit so any
// client (not just fetch) decodes the Chinese descriptions/detail strings
// correctly.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// WriteError emits the API's uniform {"error": msg} envelope.
func WriteError(w http.ResponseWriter, status int, msg string) {
	WriteJSON(w, status, map[string]any{"error": msg})
}
