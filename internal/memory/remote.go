// remote.go is the enterprise memory Engine: a ladyM-enterprise HTTP client
// (design doc §6.3.2, microservice row). woca does not embed the ladyM
// engine in server deployments — the worker talks to the ladyM API service
// over HTTP, scoped by a workspace key derived from the caller's tenancy
// context. Reads degrade to empty results on failure (Recall sits on the
// run-start path and must never block a run); writes are bounded and report
// errors for the caller's async-retry policy.
package memory

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	ladymclient "github.com/ProjAnvil/LadyM/client/golang"
	"github.com/projanvil/woca/internal/platform/tenancy"
)

// ErrIndexRequiresSandbox reports that code indexing is not available over
// the remote data-plane: per design §6.3.5 it runs inside the session
// sandbox via the ladyM CLI against the Indexer service, which arrives with
// the sandboxed executor (Phase 3).
var ErrIndexRequiresSandbox = errors.New("memory: code indexing requires the sandbox executor (remote ladyM has no index endpoint)")

// RemoteConfig configures the remote engine.
type RemoteConfig struct {
	// Endpoint is the ladyM API base URL (scheme://host:port).
	Endpoint string
	// Username/Password feed the data-plane's Basic auth (ladyM users
	// table). Both empty targets a no-auth deployment (auth=off behind a
	// private-segment gateway); tenant-scoped bearer tokens arrive with
	// ladyM P3.
	Username string
	Password string

	// RecallBudget bounds every read path (default 3s). Exceeded budgets
	// degrade to empty results, never errors.
	RecallBudget time.Duration
	// WriteTimeout bounds every write path (default 10s).
	WriteTimeout time.Duration
}

// breaker thresholds for the read path.
const (
	breakerFailures = 5
	breakerOpenFor  = 30 * time.Second
)

// timeZero is the breaker's reset point (tests use it directly).
var timeZero time.Time

// RemoteEngine implements Engine over the ladyM HTTP data-plane.
type RemoteEngine struct {
	client   *ladymclient.Client
	endpoint string

	recallBudget time.Duration
	writeTimeout time.Duration

	mu          sync.Mutex
	consecFails int
	openUntil   time.Time
}

// BuildRemote validates cfg and returns the remote engine.
func BuildRemote(cfg RemoteConfig) (*RemoteEngine, error) {
	if cfg.Endpoint == "" {
		return nil, errors.New("memory: remote engine requires ladym_endpoint")
	}
	if u, err := url.Parse(cfg.Endpoint); err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("memory: invalid ladym_endpoint %q", cfg.Endpoint)
	}
	if cfg.RecallBudget <= 0 {
		cfg.RecallBudget = 3 * time.Second
	}
	if cfg.WriteTimeout <= 0 {
		cfg.WriteTimeout = 10 * time.Second
	}
	return &RemoteEngine{
		client:       ladymclient.New(cfg.Endpoint, ladymclient.WithAuth(cfg.Username, cfg.Password)),
		endpoint:     cfg.Endpoint,
		recallBudget: cfg.RecallBudget,
		writeTimeout: cfg.WriteTimeout,
	}, nil
}

// workspaceKey maps a woca workspace into ladyM's one-dimensional workspace
// key: woca:{tenant}:{user}:{workspace} (design §6.3.4). The org level does
// not exist in woca yet. Without a tenancy context (local console paths) the
// local machine identity is used — the key still namespaces by caller, so
// cross-tenant reads cannot be assembled by omitting the context.
func workspaceKey(ctx context.Context, workspace string) string {
	tenant, user := "local", ""
	if tc, ok := tenancy.FromContext(ctx); ok {
		if tc.TenantID != "" {
			tenant = tc.TenantID
		}
		user = tc.UserID
	}
	if workspace == "" {
		workspace = "default"
	}
	return "woca:" + tenant + ":" + user + ":" + workspace
}

// userKey is the user-level float-up scope (workspace omitted): Recall also
// consults it and merges, which is how the org/user hierarchy is realized
// woca-side (ladyM stays one-dimensional; design §6.3.4).
func userKey(ctx context.Context, workspace string) string {
	key := workspaceKey(ctx, workspace)
	if i := strings.LastIndex(key, ":"); i >= 0 {
		return key[:i]
	}
	return key
}

// Recall queries the workspace scope plus the user-level float-up scope and
// merges by score. Any failure — unreachable server, timeout, open breaker —
// degrades to empty memories: the run continues (design §6.3.7).
func (e *RemoteEngine) Recall(ctx context.Context, query, workspace string, topK int) ([]Hit, error) {
	if topK <= 0 {
		topK = 8
	}
	if !e.allowRead() {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, e.recallBudget)
	defer cancel()

	primary := workspaceKey(ctx, workspace)
	scopes := []string{primary}
	if u := userKey(ctx, workspace); u != primary {
		scopes = append(scopes, u)
	}
	seen := map[string]bool{}
	var merged []Hit
	for _, scope := range scopes {
		res, err := e.client.Recall(ctx, query, ladymclient.RecallOptions{Workspace: scope, TopK: topK})
		e.recordCall(err)
		if err != nil {
			continue // degrade: empty memory for this scope
		}
		for _, r := range res.Results {
			if r.Memory == nil || seen[r.Memory.ID] {
				continue
			}
			seen[r.Memory.ID] = true
			merged = append(merged, hitFromMemory(r.Memory, r.Score))
		}
	}
	sort.SliceStable(merged, func(i, j int) bool { return merged[i].Score > merged[j].Score })
	if len(merged) > topK {
		merged = merged[:topK]
	}
	return merged, nil
}

// SearchCode is Recall restricted to code items; no user-level float-up
// (code symbols are workspace-local).
func (e *RemoteEngine) SearchCode(ctx context.Context, query, workspace string, topK int) ([]Hit, error) {
	if topK <= 0 {
		topK = 8
	}
	if !e.allowRead() {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, e.recallBudget)
	defer cancel()
	res, err := e.client.Recall(ctx, query, ladymclient.RecallOptions{
		Workspace: workspaceKey(ctx, workspace), TopK: topK, CodeOnly: true,
	})
	e.recordCall(err)
	if err != nil {
		return nil, nil // degrade
	}
	out := make([]Hit, 0, len(res.Results))
	for _, r := range res.Results {
		if r.Memory == nil {
			continue
		}
		out = append(out, hitFromMemory(r.Memory, r.Score))
	}
	return out, nil
}

// Remember stores a semantic fact through the remote attention gate. An
// empty id with a nil error means the server's gate dropped the content —
// the same contract as the embedded engine.
func (e *RemoteEngine) Remember(ctx context.Context, workspace, content string, tags []string, source string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, e.writeTimeout)
	defer cancel()
	res, err := e.client.RememberWithSource(ctx, content, source, tags, workspaceKey(ctx, workspace))
	if err != nil {
		return "", fmt.Errorf("memory: remote remember: %w", err)
	}
	if res.Dropped() {
		return "", nil
	}
	return res.ID, nil
}

// RecordEvent stores an L1 episodic tuple remotely.
func (e *RemoteEngine) RecordEvent(ctx context.Context, workspace, agentName, action, observation, outcome string) error {
	ctx, cancel := context.WithTimeout(ctx, e.writeTimeout)
	defer cancel()
	_, err := e.client.RecordEvent(ctx, agentName, action, observation, outcome, nil, workspaceKey(ctx, workspace))
	if err != nil {
		return fmt.Errorf("memory: remote record_event: %w", err)
	}
	return nil
}

// List returns recent memories newest-first.
func (e *RemoteEngine) List(ctx context.Context, workspace, layer string, limit, offset int) ([]Hit, error) {
	if !e.allowRead() {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, e.recallBudget)
	defer cancel()
	page, err := e.client.ListMemories(ctx, ladymclient.MemoryFilter{
		Workspace: workspaceKey(ctx, workspace), Layer: layer, Limit: limit, Offset: offset,
	})
	e.recordCall(err)
	if err != nil {
		return nil, fmt.Errorf("memory: remote list: %w", err)
	}
	out := make([]Hit, 0, len(page.Memories))
	for _, m := range page.Memories {
		if m == nil {
			continue
		}
		out = append(out, hitFromMemory(m, 0))
	}
	return out, nil
}

// Forget deletes one memory by id; ladyM's server-side scope enforcement is
// the authorization boundary in remote mode (design §6.3.4).
func (e *RemoteEngine) Forget(ctx context.Context, id string) error {
	ctx, cancel := context.WithTimeout(ctx, e.writeTimeout)
	defer cancel()
	if err := e.client.Forget(ctx, id); err != nil {
		return fmt.Errorf("memory: remote forget: %w", err)
	}
	return nil
}

// Stats counts memories per layer for the mapped workspace.
func (e *RemoteEngine) Stats(ctx context.Context, workspace string) (*Stats, error) {
	if !e.allowRead() {
		return &Stats{}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, e.recallBudget)
	defer cancel()
	st, err := e.client.Stats(ctx, workspaceKey(ctx, workspace))
	e.recordCall(err)
	if err != nil {
		return nil, fmt.Errorf("memory: remote stats: %w", err)
	}
	return &Stats{Total: st.TotalMemories, Layers: st.ByLayer}, nil
}

// IndexCode is not available over the remote data-plane (design §6.3.5).
func (e *RemoteEngine) IndexCode(context.Context, string, string, bool) (string, error) {
	return "", ErrIndexRequiresSandbox
}

// Consolidate triggers one System 2 cycle on the ladyM worker service.
func (e *RemoteEngine) Consolidate(ctx context.Context, workspace string) (string, error) {
	// Consolidation is the long pole — bound generously, not with the read
	// budget (the client's own CLI uses 120s for this call).
	ctx, cancel := context.WithTimeout(ctx, e.writeTimeout*6)
	defer cancel()
	res, err := e.client.Consolidate(ctx, workspaceKey(ctx, workspace))
	if err != nil {
		return "", fmt.Errorf("memory: remote consolidate: %w", err)
	}
	return fmt.Sprintf("kept_episodes=%d promoted=%d actions=%v",
		res.KeptEpisodes, res.PromotedToSemantic, res.Actions), nil
}

// Description names the engine for /memory stats output.
func (e *RemoteEngine) Description() string {
	if u, err := url.Parse(e.endpoint); err == nil && u.Host != "" {
		return "ladym-remote:" + u.Host
	}
	return "ladym-remote"
}

// Close releases resources; the HTTP client holds no state to drain.
func (e *RemoteEngine) Close() error { return nil }

// --- breaker ---------------------------------------------------------------

// allowRead reports whether a read attempt should proceed.
func (e *RemoteEngine) allowRead() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return time.Now().After(e.openUntil)
}

// recordCall maintains the consecutive-failure breaker.
func (e *RemoteEngine) recordCall(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err == nil {
		e.consecFails = 0
		return
	}
	e.consecFails++
	if e.consecFails >= breakerFailures {
		e.openUntil = time.Now().Add(breakerOpenFor)
		e.consecFails = 0
	}
}

// compile-time interface check.
var _ Engine = (*RemoteEngine)(nil)
