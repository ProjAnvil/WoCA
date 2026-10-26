package proxy

import (
	"cmp"
	cryptorand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/projanvil/woca/internal/platform/httpx"
	"github.com/projanvil/woca/internal/wocadb"
)

// WorkspaceBindingDegraded marks a binding whose executor left the ack
// liveness window (the scan flips it back to active on return).
const WorkspaceBindingDegraded = "degraded"

// errNoHealthyExecutor: no executor-role ack inside the liveness window.
var errNoHealthyExecutor = errors.New("no healthy executor available (Role=executor with a fresh ack)")

// bindBodyLimit bounds the allocate-or-bind request body: two short strings.
const bindBodyLimit = 4 << 10

// bindRequest / bindResponse are the allocate-or-bind wire forms (spec
// §3.2; the scheduler's createSession is the caller, Task 14).
type bindRequest struct {
	TenantID  string `json:"tenant_id"`
	SourceKey string `json:"source_key"`
}

type bindResponse struct {
	WorkspaceID string `json:"workspace_id"`
	ExecutorID  string `json:"executor_id"`
	State       string `json:"state"`
}

// handleBind is POST /internal/bindings: allocate-or-bind. Idempotent on
// source_key — the first caller allocates (placement policy below), every
// later caller over the same key observes the same row via
// wocadb.EnsureWorkspaceBinding's ON CONFLICT DO NOTHING + re-read, so
// concurrent allocators of one workspace converge on one ID. Re-binding an
// existing source answers from the table without re-placement (and without
// needing a healthy executor at all — the binding already exists).
func (p *Proxy) handleBind(w http.ResponseWriter, r *http.Request) {
	var req bindRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, bindBodyLimit)).Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "bindings: malformed JSON body")
		return
	}
	req.TenantID = strings.TrimSpace(req.TenantID)
	req.SourceKey = strings.TrimSpace(req.SourceKey)
	if req.TenantID == "" || req.SourceKey == "" {
		httpx.WriteError(w, http.StatusBadRequest, "bindings: tenant_id and source_key are required")
		return
	}
	if p.db == nil {
		writeUnavailable(w, http.StatusServiceUnavailable, "bindings: proxy has no database", retryAfterRouteSec)
		return
	}

	if existing, ok, err := p.db.WorkspaceBindingBySourceKey(req.SourceKey); err != nil {
		p.log.Error().Err(err).Msg("proxy: binding lookup failed")
		writeUnavailable(w, http.StatusInternalServerError, "bindings: lookup failed", retryAfterRouteSec)
		return
	} else if ok {
		httpx.WriteJSON(w, http.StatusOK, bindResponseFrom(existing))
		return
	}

	pick, err := p.pickExecutor()
	if err != nil {
		p.log.Warn().Err(err).Msg("proxy: allocate-or-bind found no healthy executor")
		writeUnavailable(w, http.StatusServiceUnavailable, err.Error(), retryAfterRouteSec)
		return
	}

	got, err := p.db.EnsureWorkspaceBinding(wocadb.WorkspaceBinding{
		WorkspaceID: mintWorkspaceID(),
		SourceKey:   req.SourceKey,
		TenantID:    req.TenantID,
		ExecutorID:  pick.ServiceID,
		// State/CreatedAt/UpdatedAt default in EnsureWorkspaceBinding.
	})
	if err != nil {
		p.log.Error().Err(err).Msg("proxy: ensure binding failed")
		writeUnavailable(w, http.StatusInternalServerError, "bindings: persist failed", retryAfterRouteSec)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, bindResponseFrom(got))
}

func bindResponseFrom(b wocadb.WorkspaceBinding) bindResponse {
	return bindResponse{WorkspaceID: b.WorkspaceID, ExecutorID: b.ExecutorID, State: b.State}
}

// pickExecutor applies the placement policy: candidate executors are the
// config-ack rows with Role=executor, a fresh ack and a routable address;
// the winner is the least loaded, then the one with the fewest existing
// bindings, then the lowest ServiceID (a total order — every replica of the
// stateless proxy picks the same executor for the same table state).
func (p *Proxy) pickExecutor() (wocadb.ConfigAck, error) {
	acks, err := p.db.ConfigAcks()
	if err != nil {
		return wocadb.ConfigAck{}, fmt.Errorf("config acks unreadable: %w", err)
	}
	bindings, err := p.db.WorkspaceBindings()
	if err != nil {
		return wocadb.ConfigAck{}, fmt.Errorf("workspace bindings unreadable: %w", err)
	}
	counts := make(map[string]int, len(bindings))
	for _, b := range bindings {
		counts[b.ExecutorID]++
	}
	now := wocadb.Now()
	candidates := make([]wocadb.ConfigAck, 0, len(acks))
	for _, a := range acks {
		if a.Role != "executor" || now-a.UpdatedAt > ackLiveWindow || strings.TrimSpace(a.Addr) == "" {
			continue
		}
		candidates = append(candidates, a)
	}
	if len(candidates) == 0 {
		return wocadb.ConfigAck{}, errNoHealthyExecutor
	}
	slices.SortFunc(candidates, func(a, b wocadb.ConfigAck) int {
		if c := cmp.Compare(a.Load, b.Load); c != 0 {
			return c
		}
		if c := cmp.Compare(counts[a.ServiceID], counts[b.ServiceID]); c != 0 {
			return c
		}
		return strings.Compare(a.ServiceID, b.ServiceID)
	})
	return candidates[0], nil
}

// scanOnce is one degraded-scan pass: every binding whose executor is not
// in the fresh-ack set flips to degraded; every degraded binding whose
// executor came back flips to active. Transitions are written only when the
// state actually changes (the 5s scan must not churn updated_at).
func (p *Proxy) scanOnce() {
	if p.db == nil {
		return
	}
	bindings, err := p.db.WorkspaceBindings()
	if err != nil {
		p.log.Warn().Err(err).Msg("proxy: degraded scan: bindings unreadable")
		return
	}
	acks, err := p.db.ConfigAcks()
	if err != nil {
		p.log.Warn().Err(err).Msg("proxy: degraded scan: acks unreadable")
		return
	}
	now := wocadb.Now()
	live := make(map[string]bool, len(acks))
	for _, a := range acks {
		if a.Role == "executor" && now-a.UpdatedAt <= ackLiveWindow {
			live[a.ServiceID] = true
		}
	}
	for _, b := range bindings {
		want := wocadb.WorkspaceBindingActive
		reason := "executor ack back in window"
		if !live[b.ExecutorID] {
			want = WorkspaceBindingDegraded
			reason = "executor ack out of 15s liveness window"
		}
		if b.State == want {
			continue
		}
		if err := p.db.SetWorkspaceBindingState(b.WorkspaceID, want); err != nil {
			p.log.Warn().Err(err).Str("workspace", b.WorkspaceID).Msg("proxy: degraded scan: state write failed")
			continue
		}
		if want == WorkspaceBindingDegraded {
			p.log.Warn().Str("workspace", b.WorkspaceID).Str("executor", b.ExecutorID).Msg("proxy: workspace degraded (executor stopped heartbeating)")
		} else {
			p.log.Info().Str("workspace", b.WorkspaceID).Str("executor", b.ExecutorID).Str("reason", reason).Msg("proxy: workspace back to active")
		}
	}
}

// writeUnavailable emits a 503-class error with a Retry-After hint.
func writeUnavailable(w http.ResponseWriter, status int, msg, retryAfterSec string) {
	w.Header().Set("Retry-After", retryAfterSec)
	httpx.WriteError(w, status, msg)
}

// mintWorkspaceID mints an unguessable workspace ID (charset [A-Za-z0-9_-]:
// the ws_ prefix plus hex).
func mintWorkspaceID() string {
	var b [12]byte
	if _, err := cryptorand.Read(b[:]); err != nil {
		return fmt.Sprintf("ws_%x", wocadb.Now())
	}
	return "ws_" + hex.EncodeToString(b[:])
}
