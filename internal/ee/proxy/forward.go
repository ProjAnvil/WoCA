package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/projanvil/woca/internal/platform/httpx"
)

// createBodyLimit bounds the one body the proxy reads: the create
// endpoint's small JSON (workspace + cwd + options). Every other endpoint's
// body streams through untouched — tar archives are 512 MiB-class and must
// never land in proxy memory.
const createBodyLimit = 1 << 20 // 1 MiB

// errCreateBodyTooLarge / errCreateBodyMalformed shape the create-path 4xx.
var (
	errCreateBodyTooLarge = errors.New("create body exceeds 1 MiB")
	errCreateBodyInvalid  = errors.New("create body is not valid JSON")
)

// targetKey carries the per-request forward target through the ReverseProxy
// Rewrite hook.
type targetKey struct{}

func withTarget(ctx context.Context, u *url.URL) context.Context {
	return context.WithValue(ctx, targetKey{}, u)
}

func targetFromCtx(ctx context.Context) *url.URL {
	u, _ := ctx.Value(targetKey{}).(*url.URL)
	return u
}

// buildReverseProxy assembles the single forwarding proxy. Rewrite resolves
// the target per request from the context (set by handleForward after
// routing); FlushInterval -1 flushes every chunk immediately so tar streams
// pass through unbuffered in both directions. Authorization passes through
// untouched (Rewrite only strips hop-by-hop and forwarding headers): the
// executor's API-key check is the last hop of the auth chain.
func (p *Proxy) buildReverseProxy() *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			u := targetFromCtx(pr.In.Context())
			if u == nil {
				return // unreachable: handleForward always sets a target
			}
			pr.SetURL(u)
			pr.Out.Host = u.Host
		},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			p.log.Warn().Err(err).Str("path", r.URL.Path).Msg("proxy: executor unreachable")
			httpx.WriteError(w, http.StatusBadGateway, "executor unreachable")
		},
	}
}

// handleForward is the /sandbox/ catch-all: policy first (auth header,
// routing key, executor admission), then byte forwarding.
func (p *Proxy) handleForward(w http.ResponseWriter, r *http.Request) {
	// Auth check: the proxy does not validate the key (the executor does),
	// but it refuses to forward unauthenticated traffic at all.
	if r.Header.Get("Authorization") == "" {
		httpx.WriteError(w, http.StatusUnauthorized, "missing Authorization header")
		return
	}

	// Routing key: the header; the create endpoint may carry it in the
	// buffered body instead (its small JSON has a workspace field).
	workspace := strings.TrimSpace(r.Header.Get(WorkspaceHeader))
	if isCreate(r) {
		fromBody, body, err := p.bufferCreateBody(r)
		if err != nil {
			status := http.StatusBadRequest
			if errors.Is(err, errCreateBodyTooLarge) {
				status = http.StatusRequestEntityTooLarge
			}
			httpx.WriteError(w, status, err.Error())
			return
		}
		if workspace == "" {
			workspace = fromBody
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
	}
	if workspace == "" {
		writeUnavailable(w, http.StatusServiceUnavailable, errMissingWorkspace.Error(), retryAfterRouteSec)
		return
	}

	target, err := p.route(workspace)
	if err != nil {
		writeUnavailable(w, http.StatusServiceUnavailable, err.Error(), retryAfterRouteSec)
		return
	}

	// Admission: the executor's bounded semaphore/queue. The tenant comes
	// from the binding (never from a request header), which is what makes
	// the per-tenant quota unforgeable.
	release, err := p.queues.acquire(r.Context(), target.binding.ExecutorID, target.binding.TenantID)
	if err != nil {
		writeUnavailable(w, http.StatusServiceUnavailable, err.Error(), retryAfterQueueSec)
		return
	}
	defer release()

	p.rp.ServeHTTP(w, r.WithContext(withTarget(r.Context(), target.url)))
}

// isCreate reports whether r is the sandbox create endpoint (the one
// request whose body carries the workspace field).
func isCreate(r *http.Request) bool {
	return r.Method == http.MethodPost && r.URL.Path == "/sandbox/create"
}

// bufferCreateBody reads (bounded) and parses the create body, returning
// the workspace field and the raw bytes so the caller can re-set the body.
// An empty body is allowed (a non-workspace-routed create); anything
// non-empty must be valid JSON.
func (p *Proxy) bufferCreateBody(r *http.Request) (string, []byte, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, createBodyLimit+1))
	if err != nil {
		return "", nil, err
	}
	if len(body) > createBodyLimit {
		return "", nil, errCreateBodyTooLarge
	}
	if len(body) == 0 {
		return "", body, nil
	}
	var parsed struct {
		Workspace string `json:"workspace"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", nil, errCreateBodyInvalid
	}
	return parsed.Workspace, body, nil
}
