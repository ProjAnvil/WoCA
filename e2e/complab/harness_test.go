//go:build complab

// complab — woca's component integration tests.
//
// A sibling of netlab (docker network-zone isolation) and sandboxlab (the
// sandbox execution chain), but with a different focus: no docker topology
// here. woca is assembled in-process the way production wires it, and each
// component is checked for actually working. Components are also exercised in
// combination (search a page, then fetch it), because green unit tests do not
// prove the pieces work when connected.
//
// Isolated behind the `complab` build tag so a plain `go test ./...` never
// touches the network:
//
//	make complab           # every component (including those needing internet)
//	make complab-offline   # only the components that work offline
//
// Design tradeoffs are written up in e2e/complab/README.md.
package complab

import (
	"context"
	"errors"
	"net"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	coretools "github.com/projanvil/langchain-golang/core/tools"

	"github.com/projanvil/woca/internal/tools"
	"github.com/projanvil/woca/internal/websearch"
)

// offlineEnv lets `make complab-offline` turn the networked components off
// explicitly, rather than inferring it from a dial timeout (which is slow on a
// restricted network).
const offlineEnv = "WOCA_COMPLAB_OFFLINE"

// requireNetwork skips a component that cannot run without outbound internet.
//
// Skip rather than fail: no internet is an environment problem, not a broken
// component. "Internet works but every free backend refused us" is a different
// matter — that is a real finding and must fail. See explainSearchFailure.
func requireNetwork(t *testing.T) {
	t.Helper()
	if os.Getenv(offlineEnv) == "1" {
		t.Skipf("%s=1 — networked components disabled by configuration", offlineEnv)
	}
	conn, err := net.DialTimeout("tcp", "go.dev:443", 5*time.Second)
	if err != nil {
		t.Skipf("no outbound connectivity (dial go.dev:443: %v) — skipping networked component", err)
	}
	_ = conn.Close()
}

// newRegistry builds the production tool registry over a throwaway workspace.
// Going through the registry rather than calling package internals means the
// test exercises the same tool set the agent actually gets.
func newRegistry(t *testing.T) *tools.Registry {
	t.Helper()
	return tools.NewRegistry(t.TempDir())
}

// toolNamed fetches a tool from the registry, failing if it is not exposed.
func toolNamed(t *testing.T, reg *tools.Registry, name string) coretools.Tool {
	t.Helper()
	tool, ok := reg.Get(name)
	if !ok {
		t.Fatalf("registry does not expose tool %q", name)
	}
	return tool
}

// invoke runs a tool the way the agent does and returns its content.
func invoke(t *testing.T, tool coretools.Tool, args map[string]any) (string, error) {
	t.Helper()
	res, err := tool.Invoke(context.Background(), args)
	if err != nil {
		return "", err
	}
	return res.Content, nil
}

// ───────────────── event capture ─────────────────

// eventLog collects router events so a failure can say which path was serving.
//
// The suite deliberately does NOT assert which provider served a query — that
// is the unit tests' job, and free backends come and go with the network. Here
// the events exist to make failures diagnosable.
type eventLog struct {
	mu     sync.Mutex
	events []websearch.Event
}

func (l *eventLog) add(e websearch.Event) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
}

// servedBy returns the last provider that succeeded, or "" if none did.
func (l *eventLog) servedBy() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := len(l.events) - 1; i >= 0; i-- {
		if l.events[i].Kind == websearch.EventSuccess {
			return l.events[i].Provider
		}
	}
	return ""
}

// summary renders a one-line human-readable event trail.
func (l *eventLog) summary() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	parts := make([]string, 0, len(l.events))
	for _, e := range l.events {
		p := e.Provider
		if p == "" {
			p = "-"
		}
		parts = append(parts, p+"/"+string(e.Kind))
	}
	return strings.Join(parts, " -> ")
}

// installEventLog installs the process-wide event hook and restores it after.
func installEventLog(t *testing.T) *eventLog {
	t.Helper()
	l := &eventLog{}
	websearch.SetEventHook(l.add)
	t.Cleanup(func() { websearch.SetEventHook(nil) })
	return l
}

// ───────────────── explaining a search failure ─────────────────

// explainSearchFailure turns a search failure into something actionable, then
// fails the test.
//
// This is the one networked situation that must FAIL rather than skip: internet
// is reachable but every free backend refused us, which means this network (the
// typical datacenter-IP case) cannot use the default chain and needs a
// self-hosted backend.
func explainSearchFailure(t *testing.T, err error, log *eventLog) {
	t.Helper()
	t.Logf("event trail: %s", log.summary())
	if errors.Is(err, websearch.ErrRateLimited) {
		t.Fatalf("every free backend was throttled or blocked (%v).\n"+
			"DuckDuckGo blocks by IP reputation and fails deterministically from datacenter "+
			"ranges (CI runners, enterprise workers, containers).\n"+
			"On this network, open Settings -> Web search and either point the searxng "+
			"backend at a reachable instance (for example https://searx.internal.example.com) "+
			"or enable the Tavily keyless backend.",
			err)
	}
	if errors.Is(err, websearch.ErrNoBackend) {
		t.Fatalf("no usable backend: %v — enable at least one backend under "+
			"Settings -> Web search", err)
	}
	t.Fatalf("search failed: %v", err)
}

// ───────────────── parsing results ─────────────────

// resultURLRe matches the indented URL lines websearch.Format renders.
var resultURLRe = regexp.MustCompile(`(?m)^\s{3}(https?://\S+)\s*$`)

// resultURLs extracts result URLs from tool output.
func resultURLs(content string) []string {
	matches := resultURLRe.FindAllStringSubmatch(content, -1)
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, m[1])
	}
	return out
}

// countResults counts result entries in formatted output (lines starting "1. ").
func countResults(content string) int {
	n := 0
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimLeft(line, " ")
		if len(trimmed) == 0 || trimmed[0] < '0' || trimmed[0] > '9' {
			continue
		}
		if i := strings.Index(trimmed, ". "); i > 0 {
			n++
		}
	}
	return n
}
