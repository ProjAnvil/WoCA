package relay

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// MaxClientBody is the client→bridge request body cap (design §6.3: 4
// MB, 413 beyond; streaming uploads are YAGNI).
const MaxClientBody = 4 << 20

// bridgeOfflineRetryAfter is the Retry-After hint when no bridge
// generation is online (design §9 error matrix).
const bridgeOfflineRetryAfter = 5

// forward relays one authenticated client request through the parking
// plane to the bridge and streams its answer back (design §6.2/§6.3).
func (rl *Relay) forward(w http.ResponseWriter, r *http.Request, sess Session) {
	if !rl.gens.Online(sess.KeyID) {
		w.Header().Set("Retry-After", strconv.Itoa(bridgeOfflineRetryAfter))
		WriteError(w, http.StatusServiceUnavailable, CodeBridgeOffline, "bridge offline")
		return
	}
	body, err := readCappedBody(w, r)
	if err != nil {
		WriteError(w, http.StatusRequestEntityTooLarge, CodeUnauthorized, "request body too large")
		return
	}
	headers := r.Header.Clone()
	// The relay's own session credentials mean nothing to the bridge and
	// must never cross the tunnel (design §6.4).
	headers.Del("Cookie")
	headers.Del("Authorization")
	resp, done, failed, err := rl.park.Park(r.Context(), sess.KeyID, ParkedRequest{
		ID:       newRequestID(),
		Method:   r.Method,
		Path:     r.URL.Path,
		Query:    r.URL.RawQuery,
		ClientIP: rl.auth.ClientIP(r),
		Headers:  headers,
		Body:     body,
	})
	if errors.Is(err, ErrQueueFull) {
		WriteError(w, http.StatusServiceUnavailable, CodeBridgeOffline, "bridge busy")
		return
	}
	if err != nil {
		// Client disconnect or revocation drain — nothing left to write.
		return
	}
	defer done()

	out := w.Header()
	for k, vs := range resp.Headers {
		for _, v := range vs {
			out.Add(k, v)
		}
	}
	eventStream := strings.HasPrefix(out.Get("Content-Type"), "text/event-stream")
	if eventStream {
		// Keep intermediary proxies from buffering the stream (design §6.3).
		out.Set("X-Accel-Buffering", "no")
	}
	w.WriteHeader(resp.Status)
	defer func() { _ = resp.Body.Close() }()
	flusher, _ := w.(http.Flusher)
	// Client disconnect mid-stream: closing the body unblocks Read so the
	// loop exits promptly instead of pinning the bridge's respond.
	stopWatch := context.AfterFunc(r.Context(), func() { _ = resp.Body.Close() })
	defer stopWatch()
	_ = streamChunks(w, flusher, resp.Body)
	// Revocation mid-stream: inject the revocation event before closing
	// (design §5.2). The frame MUST use the SPA's own vocabulary —
	// `event: auth_revoked` with {"code":"revoked"} — which is exactly
	// what web/src/api/streaming.ts:151-159 switches on (constant
	// EventAuthRevoked, internal/server/ssevents.go:23); any other event
	// name and the revoked notice never fires. failCause is
	// deterministic; read-error flavor is not.
	select {
	case cause := <-failed:
		if eventStream && errors.Is(cause, ErrRevoked) {
			_, _ = fmt.Fprint(w, "event: auth_revoked\ndata: {\"code\":\"revoked\"}\n\n")
			if flusher != nil {
				flusher.Flush()
			}
		}
	default:
	}
}

// streamChunks copies body to w, flushing after every chunk — no
// aggregation, no buffering (design §6.3).
func streamChunks(w http.ResponseWriter, flusher http.Flusher, body io.Reader) error {
	buf := make([]byte, 32<<10)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return werr
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// readCappedBody enforces MaxClientBody (413 beyond, design §6.3).
func readCappedBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	defer func() { _ = r.Body.Close() }()
	return io.ReadAll(http.MaxBytesReader(w, r.Body, MaxClientBody))
}

// newRequestID mints a parked-request id (128-bit CSPRNG, base64url).
func newRequestID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic(err) // crypto/rand failure is unrecoverable
	}
	return base64.RawURLEncoding.EncodeToString(raw[:])
}
