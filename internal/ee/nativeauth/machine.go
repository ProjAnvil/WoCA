// machine.go is the machine-channel variant of the native credential
// surface, mounted on the service-plane gateway listener beside authcore
// (/gw/native/*). The edge — the auth enforcement point for public native
// clients — calls it home: activation is proxied, sessions are introspected
// (with the DPoP thumbprint so the edge verifies proofs at its own origin),
// and revocation is edge-initiated after the edge checks the proof. The
// private network is the boundary, like every other /gw route.
package nativeauth

import (
	"encoding/json"
	"net/http"

	"github.com/projanvil/woca/internal/ee/tenant"
	"github.com/projanvil/woca/internal/platform/httpx"
)

// MachineHandler serves the edge-facing machine channel of the native
// surface.
func MachineHandler(na *Surface) http.Handler {
	mux := http.NewServeMux()
	decode := func(w http.ResponseWriter, r *http.Request, dst any) bool {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(dst); err != nil {
			httpx.WriteJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
			return false
		}
		return true
	}
	mux.HandleFunc("POST /gw/native/activate", func(w http.ResponseWriter, r *http.Request) {
		// No per-caller limiter here: the caller is the edge, which rate
		// limits each public client itself (a limiter keyed on the edge's
		// address would throttle all clients collectively).
		var req activateRequest
		if !decode(w, r, &req) {
			return
		}
		out, err := na.activate(r.Context(), req)
		if err != nil {
			httpx.WriteJSON(w, nativeActivateStatus(err), map[string]string{"error": nativeActivateMessage(err)})
			return
		}
		httpx.WriteJSON(w, http.StatusOK, out)
	})
	mux.HandleFunc("POST /gw/native/introspect", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Token string `json:"token"`
		}
		if !decode(w, r, &req) {
			return
		}
		sess, state, err := na.Store.LookupSessionState(r.Context(), req.Token)
		if err != nil {
			httpx.WriteJSON(w, http.StatusInternalServerError, map[string]string{"error": "unavailable"})
			return
		}
		switch state {
		case tenant.SessionActive:
			_ = na.Store.TouchSession(r.Context(), req.Token, na.now().UnixMilli())
			httpx.WriteJSON(w, http.StatusOK, introspectResponse{
				Active:          true,
				TenantID:        sess.TenantID,
				UserID:          sess.UserID,
				Role:            sess.Role,
				ExpiresAt:       sess.ExpiresAt,
				DeviceID:        sess.DeviceID,
				DPoPThumbprint:  sess.DPoPThumbprint,
				LastValidatedAt: sess.LastValidatedAt,
			})
		case tenant.SessionRevoked:
			httpx.WriteJSON(w, http.StatusOK, introspectResponse{Code: "revoked"})
		default:
			httpx.WriteJSON(w, http.StatusOK, introspectResponse{})
		}
	})
	mux.HandleFunc("POST /gw/native/revoke", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Token string `json:"token"`
		}
		if !decode(w, r, &req) {
			return
		}
		if err := na.Store.RevokeSession(r.Context(), req.Token); err != nil {
			httpx.WriteJSON(w, http.StatusInternalServerError, map[string]string{"error": "unavailable"})
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	return mux
}

// introspectResponse answers the edge: on success it carries the actor
// fields plus the DPoP thumbprint (the edge verifies proofs against its own
// origin, where the htu was signed); on failure the code distinguishes
// revocation the same way authcore's introspect does.
type introspectResponse struct {
	Active          bool   `json:"active"`
	Code            string `json:"code,omitempty"`
	TenantID        string `json:"tenant_id,omitempty"`
	UserID          string `json:"user_id,omitempty"`
	Role            string `json:"role,omitempty"`
	ExpiresAt       int64  `json:"expires_at,omitempty"`
	DeviceID        string `json:"device_id,omitempty"`
	DPoPThumbprint  string `json:"dpop_thumbprint,omitempty"`
	LastValidatedAt int64  `json:"last_validated_at"`
}
