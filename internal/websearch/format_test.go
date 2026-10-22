package websearch

import "testing"

func TestFormat(t *testing.T) {
	got := Format([]Result{
		{Title: "First", URL: "https://a.example", Snippet: "one"},
		{Title: "Second", URL: "https://b.example", Snippet: "two"},
	})
	want := "1. First\n   https://a.example\n   one\n2. Second\n   https://b.example\n   two\n"
	if got != want {
		t.Fatalf("Format = %q, want %q", got, want)
	}
}

func TestFormatSkipsEmptySnippet(t *testing.T) {
	got := Format([]Result{{Title: "T", URL: "https://a.example"}})
	want := "1. T\n   https://a.example\n"
	if got != want {
		t.Fatalf("Format = %q, want %q", got, want)
	}
}

// TestFormatOmitsContent: body excerpts are populated by providers but must not
// reach the model through search output — that is WebFetch's job, and it has a
// budget for it.
func TestFormatOmitsContent(t *testing.T) {
	got := Format([]Result{{Title: "T", URL: "https://a.example", Snippet: "s", Content: "long body"}})
	if want := "1. T\n   https://a.example\n   s\n"; got != want {
		t.Fatalf("Format = %q, want %q (content must not be rendered)", got, want)
	}
}

func TestFormatEmpty(t *testing.T) {
	if got := Format(nil); got != "" {
		t.Fatalf("Format(nil) = %q, want empty", got)
	}
}
