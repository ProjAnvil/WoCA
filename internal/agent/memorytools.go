package agent

import (
	"context"
	"fmt"
	"strings"

	coretools "github.com/projanvil/langchain-golang/core/tools"

	"github.com/projanvil/woca/internal/memory"
)

// memoryTools builds the memory-facing tool set. Subagents get the read-only
// subset (reads shared, writes governed by the main agent).
func (a *Agent) memoryTools() []coretools.Tool {
	if a.MemoryEngine() == nil {
		return nil
	}
	readonly := a.subagent

	type recallArgs struct {
		Query string `json:"query"`
		TopK  int    `json:"top_k,omitempty"`
	}
	type rememberArgs struct {
		Content string   `json:"content"`
		Tags    []string `json:"tags,omitempty"`
	}
	type indexArgs struct {
		Root  string `json:"root,omitempty"`
		Force bool   `json:"force,omitempty"`
	}

	var out []coretools.Tool

	recall, err := coretools.FromFunc("recall_memory",
		"Retrieve relevant long-term memories (facts, decisions, insights, predictions, playbooks).\n\n"+
			"Call BEFORE answering when prior context, past decisions, or how-to knowledge may be relevant. "+
			"One line per hit, or \"(no hits)\".",
		func(ctx context.Context, in recallArgs) (coretools.Result, error) {
			e := a.MemoryEngine()
			if e == nil {
				return coretools.Result{Content: "memory disabled"}, nil
			}
			hits, err := e.Recall(ctx, in.Query, a.memoryWorkspaceCtx(ctx), in.TopK)
			if err != nil {
				return coretools.Result{}, err
			}
			return coretools.Result{Content: formatMemoryHits(hits)}, nil
		})
	if err != nil {
		panic(err) // static schema reflection; unreachable unless the args struct is invalid
	}
	out = append(out, recall)

	search, err := coretools.FromFunc("search_code",
		"Search indexed source symbols/files. Use for 'how does X work in the codebase' "+
			"against previously indexed source (index_code). \"(no hits)\" usually means the root was "+
			"never indexed — call index_code first. Prefer Grep for literal searches of the live tree.",
		func(ctx context.Context, in recallArgs) (coretools.Result, error) {
			e := a.MemoryEngine()
			if e == nil {
				return coretools.Result{Content: "memory disabled"}, nil
			}
			hits, err := e.SearchCode(ctx, in.Query, a.memoryWorkspaceCtx(ctx), in.TopK)
			if err != nil {
				return coretools.Result{}, err
			}
			return coretools.Result{Content: formatMemoryHits(hits)}, nil
		})
	if err != nil {
		panic(err)
	}
	out = append(out, search)

	if !readonly {
		remember, err := coretools.FromFunc("remember_fact",
			"Persist a durable fact worth recalling later (user preference, key decision, verified answer). "+
				"Do NOT store ephemeral/transactional state.\n\n"+
				"Subject to the attention gate: if the result says 'dropped by the attention gate', "+
				"the content was judged a duplicate or noise — do not retry or rephrase it.",
			func(ctx context.Context, in rememberArgs) (coretools.Result, error) {
				e := a.MemoryEngine()
				if e == nil {
					return coretools.Result{Content: "memory disabled"}, nil
				}
				id, err := e.Remember(ctx, a.memoryWorkspaceCtx(ctx), in.Content, in.Tags, "woca:tool")
				if err != nil {
					return coretools.Result{}, err
				}
				if id == "" {
					return coretools.Result{Content: "dropped by the attention gate (duplicate or noise)"}, nil
				}
				return coretools.Result{Content: fmt.Sprintf("stored id=%s", id)}, nil
			})
		if err != nil {
			panic(err)
		}
		out = append(out, remember)

		index, err := coretools.FromFunc("index_code",
			"Index a source root into memory (tree-sitter symbols + cross-refs) so search_code can find them. "+
				"force=true reindexes unchanged files too. Incremental by default.",
			func(ctx context.Context, in indexArgs) (coretools.Result, error) {
				e := a.MemoryEngine()
				if e == nil {
					return coretools.Result{Content: "memory disabled"}, nil
				}
				root := in.Root
				if root == "" {
					root = a.workdir
				}
				report, err := e.IndexCode(ctx, root, a.memoryWorkspaceCtx(ctx), in.Force)
				if err != nil {
					return coretools.Result{}, err
				}
				return coretools.Result{Content: report}, nil
			})
		if err != nil {
			panic(err)
		}
		out = append(out, index)
	}
	return out
}

func formatMemoryHits(hits []memory.Hit) string {
	if len(hits) == 0 {
		return "(no hits)"
	}
	lines := make([]string, 0, len(hits))
	for _, h := range hits {
		lines = append(lines, fmt.Sprintf("[%s|%s|%.2f] %s", h.Layer, h.Type, h.Score, strings.TrimSpace(h.Summary)))
	}
	return strings.Join(lines, "\n")
}
