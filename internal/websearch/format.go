package websearch

import (
	"fmt"
	"strings"
)

// Format renders results as the compact numbered list the model reads:
//
//  1. Title
//     https://url
//     snippet
//
// Result.Content is deliberately NOT rendered. Free backends return body
// excerpts that would multiply the tool's token cost for every query; when we
// want bodies we will route them through WebFetch, which has its own budget.
func Format(results []Result) string {
	var b strings.Builder
	for i, r := range results {
		fmt.Fprintf(&b, "%d. %s\n   %s\n", i+1, r.Title, r.URL)
		if s := collapseSpace(r.Snippet); s != "" {
			fmt.Fprintf(&b, "   %s\n", s)
		}
	}
	return b.String()
}
