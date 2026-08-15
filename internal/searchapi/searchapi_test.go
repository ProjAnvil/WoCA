package searchapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestSearchPlaceholderQueryEncoding(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.RequestURI()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"results": []any{}})
	}))
	defer srv.Close()

	c := New(Config{Endpoint: srv.URL + "/search?q={q}"})
	if _, err := c.Search(context.Background(), "hello world & more"); err != nil {
		t.Fatalf("Search: %v", err)
	}
	want := "/search?q=" + url.QueryEscape("hello world & more")
	if gotPath != want {
		t.Fatalf("path = %q, want %q", gotPath, want)
	}
}

func TestSearchAppendsQueryParam(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.RequestURI()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"results": []any{}})
	}))
	defer srv.Close()

	c := New(Config{Endpoint: srv.URL})
	if _, err := c.Search(context.Background(), "golang"); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if gotPath != "/?q=golang" {
		t.Fatalf("path = %q, want %q", gotPath, "/?q=golang")
	}
}

func TestSearchAuthorizationHeader(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"results": []any{}})
	}))
	defer srv.Close()

	c := New(Config{Endpoint: srv.URL, APIKey: "secret-key"})
	if _, err := c.Search(context.Background(), "q"); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if gotAuth != "Bearer secret-key" {
		t.Fatalf("Authorization = %q, want %q", gotAuth, "Bearer secret-key")
	}
}

func TestSearchNoAuthorizationHeaderWithoutKey(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"results": []any{}})
	}))
	defer srv.Close()

	c := New(Config{Endpoint: srv.URL})
	if _, err := c.Search(context.Background(), "q"); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if gotAuth != "" {
		t.Fatalf("Authorization = %q, want empty", gotAuth)
	}
}

func TestSearchParsesResults(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{
				{"title": "One", "url": "https://one.example", "snippet": "first"},
				{"title": "Two", "url": "https://two.example", "description": "second"},
			},
		})
	}))
	defer srv.Close()

	c := New(Config{Endpoint: srv.URL})
	results, err := c.Search(context.Background(), "q")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	want := []Result{
		{Title: "One", URL: "https://one.example", Snippet: "first"},
		{Title: "Two", URL: "https://two.example", Snippet: "second"},
	}
	if len(results) != len(want) {
		t.Fatalf("got %d results, want %d", len(results), len(want))
	}
	for i := range want {
		if results[i] != want[i] {
			t.Fatalf("results[%d] = %+v, want %+v", i, results[i], want[i])
		}
	}
}

func TestSearchAcceptsItemsAlias(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"items": []map[string]any{
				{"title": "Only", "url": "https://only.example", "snippet": "snippet"},
			},
		})
	}))
	defer srv.Close()

	c := New(Config{Endpoint: srv.URL})
	results, err := c.Search(context.Background(), "q")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if results[0].Title != "Only" {
		t.Fatalf("results[0].Title = %q, want %q", results[0].Title, "Only")
	}
}

func TestSearchTruncatesToMaxResults(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		results := make([]map[string]any, 0, 5)
		for i := 0; i < 5; i++ {
			results = append(results, map[string]any{
				"title": "T", "url": "https://x.example", "snippet": "s",
			})
		}
		json.NewEncoder(w).Encode(map[string]any{"results": results})
	}))
	defer srv.Close()

	c := New(Config{Endpoint: srv.URL, MaxResults: 2})
	results, err := c.Search(context.Background(), "q")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
}

func TestSearchNon200ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := New(Config{Endpoint: srv.URL})
	if _, err := c.Search(context.Background(), "q"); err == nil {
		t.Fatal("expected error, got nil")
	} else if !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("error = %q, want it to contain %q", err.Error(), "HTTP 500")
	}
}

func TestFormat(t *testing.T) {
	results := []Result{
		{Title: "First", URL: "https://a.example", Snippet: "one"},
		{Title: "Second", URL: "https://b.example", Snippet: "two"},
	}
	got := Format(results)
	want := "1. First\n   https://a.example\n   one\n2. Second\n   https://b.example\n   two\n"
	if got != want {
		t.Fatalf("Format = %q, want %q", got, want)
	}
}

func TestFormatEmpty(t *testing.T) {
	if got := Format(nil); got != "" {
		t.Fatalf("Format(nil) = %q, want empty", got)
	}
}
