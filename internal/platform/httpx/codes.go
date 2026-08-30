// Package httpx: error-code registry (api-standard.md §1). Codes are the
// stable, non-localized identity of an error — messages stay English and
// the frontend maps code → i18n key. New codes are born here and nowhere
// else; segments follow the standard (S-B business, S-E edge, S-C config
// plane, S-O ops console, S-G machine channel, S-W worker, C-N native).
package httpx

import "net/http"

// Common identity codes (0001–0099) — every segment uses the same
// numbers for the same meanings.
const (
	// CodeUnauthorized: missing or malformed credential.
	CodeUnauthorized = "0001"
	// CodeCredentialRevoked: the credential was valid once; revocation is
	// terminal (no grace, no fallback).
	CodeCredentialRevoked = "0002"
	// CodeForbidden: authenticated, but the scope/tenant gate said no.
	CodeForbidden = "0003"
	// CodeNotFound: the addressed resource does not exist.
	CodeNotFound = "0004"
)

// Common request codes (0100–0199).
const (
	CodeInvalidRequest = "0100"
	CodeMalformedJSON  = "0101"
)

// Common internal/downstream codes (0500–0599).
const (
	CodeInternal         = "0500"
	CodePlaneUnreachable = "0502" // edge → core plane forward failed
	CodeStarting         = "0503" // surface is booting
)

// Segment prefixes.
const (
	SegBusiness = "S-B" // server core + bridge extras
	SegEdge     = "S-E"
	SegConfig   = "S-C" // config plane + console auth
	SegOps      = "S-O"
	SegGateway  = "S-G" // machine channel: entrygw + authcore
	SegWorker   = "S-W"
	SegClientN  = "C-N" // native client
)

// CodedError carries a segment-scoped code with its HTTP status.
type CodedError struct {
	Status  int
	Segment string // one of the Seg* constants
	Code    string // the numeric registry value, e.g. CodeUnauthorized
	Message string // English; never localized server-side
}

// ErrorID renders the wire form (e.g. "S-E-0001").
func (e CodedError) ErrorID() string { return e.Segment + "-" + e.Code }

// WriteCoded emits the standard envelope:
//
//	{"error": "<english message>", "code": "S-E-0001"}
//
// The code field is always present on errors (api-standard.md §1.1).
func WriteCoded(w http.ResponseWriter, e CodedError) {
	WriteJSON(w, e.Status, map[string]string{"error": e.Message, "code": e.ErrorID()})
}

// WriteCodedMsg is the common shorthand: segment + numeric code + status
// implied by the code family.
func WriteCodedMsg(w http.ResponseWriter, status int, segment, code, message string) {
	WriteCoded(w, CodedError{Status: status, Segment: segment, Code: code, Message: message})
}
