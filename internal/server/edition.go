// edition.go declares the enterprise seams the shared server surface
// exposes: the run-dispatch message plane and the native-client credential
// surface. Both are optional injections (nil keeps the personal-edition
// posture) and both are implemented under internal/ee — the server core
// never imports the enterprise tree.
package server

import (
	"net/http"

	"github.com/projanvil/woca/internal/gateway"
)

// MsgPlane is the run-dispatch notification plane overlaying the PG run
// queue: Notify wakes idle workers about a queued run, Wake delivers
// config-plane push signals (consumed by the settings watcher), and Close
// releases the underlying connections. Implemented by internal/ee/msgplane
// via internal/ee/edition; nil keeps pure-PG dispatch (poll-only).
type MsgPlane interface {
	// Notify announces a queued run so idle workers claim it instantly.
	Notify(runID string)
	// Wake returns the config-plane push channel (nil = poll only).
	Wake() <-chan struct{}
	// Close tears the plane down.
	Close()
}

// NativeAuth is the native-client credential surface: RegisterRoutes mounts
// /api/native/* (activation, session info, logout) and SessionAuthenticator
// returns the DPoP session verifier chained into the gateway middleware.
// Implemented by internal/ee/nativeauth; nil disables the surface (the
// personal edition default).
type NativeAuth interface {
	// RegisterRoutes mounts the native-client endpoints on mux.
	RegisterRoutes(mux *http.ServeMux)
	// SessionAuthenticator returns the DPoP session verifier.
	SessionAuthenticator() gateway.SessionAuthenticator
}
