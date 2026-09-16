package relay

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"
)

// Tunnel wire vocabulary (design §6.1). internal/relayclient carries its
// own copies of these constants — the two packages stay independently
// compilable (the same discipline as the enterprise bridge/edge pair).
const (
	GenerationHeader = "X-Woca-Relay-Generation"
	StatusHeader     = "X-Woca-Relay-Status"
	HeadersHeader    = "X-Woca-Relay-Headers"
	RemoteHeader     = "X-Woca-Relay-Remote"
)

// DefaultPollWait bounds one long poll when the bridge does not pass
// ?wait= (design §6.1: 25s).
const DefaultPollWait = 25 * time.Second

// TunnelService owns the bridge-facing endpoints: GET /tunnel/poll and
// POST /tunnel/respond/{id}.
type TunnelService struct {
	keys *KeyStore
	park *ParkingQueue
	gens *GenerationRegistry
	log  zerolog.Logger
	// OnBridgeChange fires on generation adopt (up=true) and stale
	// eviction (up=false) — the revocation task hooks the audit log here.
	OnBridgeChange func(keyID int64, up bool)
}

// NewTunnelService builds the tunnel plane.
func NewTunnelService(keys *KeyStore, park *ParkingQueue, gens *GenerationRegistry, log zerolog.Logger) *TunnelService {
	return &TunnelService{keys: keys, park: park, gens: gens, log: log}
}

// pollPayload is the parked request's wire shape (the 200 body of
// /tunnel/poll).
type pollPayload struct {
	ID      string              `json:"id"`
	Method  string              `json:"method"`
	Path    string              `json:"path"`
	Query   string              `json:"query"`
	Client  string              `json:"client"`
	Headers map[string][]string `json:"headers"`
	Body    string              `json:"body"` // base64 standard encoding
}

// authenticate resolves the Bearer key for a tunnel request: 401 for
// unknown keys, 410 {"code":"revoked"} for revoked ones (design §9).
func (t *TunnelService) authenticate(w http.ResponseWriter, r *http.Request) (Key, bool) {
	h := r.Header.Get("Authorization")
	if len(h) <= 7 || !strings.EqualFold(h[:7], "bearer ") {
		WriteError(w, http.StatusUnauthorized, CodeUnauthorized, "missing bearer key")
		return Key{}, false
	}
	key, err := t.keys.LookupKey(r.Context(), strings.TrimSpace(h[7:]))
	if errors.Is(err, ErrKeyRevoked) {
		WriteError(w, http.StatusGone, CodeRevoked, "key revoked")
		return Key{}, false
	}
	if err != nil {
		WriteError(w, http.StatusUnauthorized, CodeUnauthorized, "unknown key")
		return Key{}, false
	}
	return key, true
}

// HandlePoll implements GET /tunnel/poll?wait=25s (design §6.1): Bearer
// key + generation header, generation claim (409 on a competing live
// generation), then a long-poll take — 204 on timeout, 410 instantly on
// revocation wakeup.
func (t *TunnelService) HandlePoll(w http.ResponseWriter, r *http.Request) {
	key, ok := t.authenticate(w, r)
	if !ok {
		return
	}
	gen := r.Header.Get(GenerationHeader)
	if gen == "" {
		WriteError(w, http.StatusBadRequest, CodeUnauthorized, "missing "+GenerationHeader)
		return
	}
	adopted, evicted, err := t.gens.Claim(key.ID, gen)
	if errors.Is(err, ErrGenerationConflict) {
		WriteError(w, http.StatusConflict, CodeUnauthorized, "another bridge generation is live")
		return
	}
	if err != nil {
		WriteError(w, http.StatusInternalServerError, CodeUnauthorized, "generation claim failed")
		return
	}
	if t.OnBridgeChange != nil {
		if evicted {
			t.OnBridgeChange(key.ID, false)
		}
		if adopted {
			t.OnBridgeChange(key.ID, true)
		}
	}
	t.keys.TouchKey(r.Context(), key.ID)

	wait := DefaultPollWait
	if raw := r.URL.Query().Get("wait"); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 && d <= time.Minute {
			wait = d
		}
	}
	pollCtx, cancel := context.WithCancelCause(r.Context())
	defer cancel(nil)
	unregister, ok := t.gens.RegisterPoll(key.ID, gen, cancel)
	if !ok {
		WriteError(w, http.StatusConflict, CodeUnauthorized, "generation superseded")
		return
	}
	defer unregister()

	req, err := t.park.Take(pollCtx, key.ID, gen, wait)
	if errors.Is(err, ErrTakeTimeout) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err != nil {
		if errors.Is(context.Cause(pollCtx), ErrRevoked) {
			// Revocation wakeup: 410 immediately, not after the wait
			// window (design §5.1).
			WriteError(w, http.StatusGone, CodeRevoked, "key revoked")
			return
		}
		// The bridge hung up mid-poll: nothing to write.
		return
	}
	writeJSON(w, http.StatusOK, pollPayload{
		ID:      req.ID,
		Method:  req.Method,
		Path:    req.Path,
		Query:   req.Query,
		Client:  req.ClientIP,
		Headers: req.Headers,
		Body:    base64.StdEncoding.EncodeToString(req.Body),
	})
}

// HandleRespond implements POST /tunnel/respond/{id} (design §6.1/§6.3):
// the id must belong to this Bearer key's current generation (404
// otherwise), headers arrive base64-JSON in X-Woca-Relay-Headers with
// hop-by-hop filtering, and the body streams to the parked client.
func (t *TunnelService) HandleRespond(w http.ResponseWriter, r *http.Request) {
	key, ok := t.authenticate(w, r)
	if !ok {
		return
	}
	gen := r.Header.Get(GenerationHeader)
	if gen == "" || !t.gens.Current(key.ID, gen) {
		WriteError(w, http.StatusNotFound, CodeUnauthorized, "unknown parked request")
		return
	}
	t.keys.TouchKey(r.Context(), key.ID)

	status, err := strconv.Atoi(r.Header.Get(StatusHeader))
	if err != nil || status < 100 || status > 999 {
		WriteError(w, http.StatusBadRequest, CodeUnauthorized, "invalid "+StatusHeader)
		return
	}
	headers, err := decodeHeaders(r.Header.Get(HeadersHeader))
	if err != nil {
		WriteError(w, http.StatusBadRequest, CodeUnauthorized, "invalid "+HeadersHeader)
		return
	}
	streamDone, err := t.park.Complete(key.ID, gen, r.PathValue("id"), &ParkedResponse{
		Status:  status,
		Headers: headers,
		Body:    r.Body,
	})
	if errors.Is(err, ErrClientGone) {
		WriteError(w, http.StatusGone, CodeUnauthorized, "client disconnected")
		return
	}
	if err != nil {
		WriteError(w, http.StatusNotFound, CodeUnauthorized, "unknown parked request")
		return
	}
	// Stay attached until the client side finishes the body — returning
	// early would truncate the stream. Revocation closes streamDone and
	// the body close unsticks the parked client (design §5).
	select {
	case <-streamDone:
		w.WriteHeader(http.StatusOK)
	case <-r.Context().Done():
	}
}

// decodeHeaders restores the base64-JSON header snapshot and strips
// hop-by-hop headers.
func decodeHeaders(raw string) (http.Header, error) {
	h := http.Header{}
	if raw == "" {
		return h, nil
	}
	data, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, err
	}
	var m map[string][]string
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	for k, vs := range m {
		for _, v := range vs {
			h.Add(k, v)
		}
	}
	StripHopByHop(h)
	return h, nil
}

// StripHopByHop removes connection-specific headers in place (design
// §6.3: Connection, Keep-Alive, Transfer-Encoding, TE, Trailer, Upgrade —
// plus anything the Connection token names. woca's SSE path sets
// Connection: keep-alive; forwarding it verbatim to the browser is a
// protocol violation).
func StripHopByHop(h http.Header) {
	for _, v := range h.Values("Connection") {
		for name := range strings.SplitSeq(v, ",") {
			h.Del(strings.TrimSpace(name))
		}
	}
	for _, name := range []string{"Connection", "Keep-Alive", "Transfer-Encoding", "TE", "Trailer", "Upgrade"} {
		h.Del(name)
	}
}
