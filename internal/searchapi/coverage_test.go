package searchapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSearchErrorPaths(t *testing.T) {
	ctx := context.Background()

	// Non-200 status.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	c := New(Config{Endpoint: srv.URL + "/q={q}"})
	if _, err := c.Search(ctx, "x"); err == nil {
		t.Fatal("expected status error")
	}
	srv.Close()

	// Undecodable body.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "not json")
	}))
	defer srv2.Close()
	c2 := New(Config{Endpoint: srv2.URL + "/q={q}"})
	if _, err := c2.Search(ctx, "x"); err == nil {
		t.Fatal("expected decode error")
	}

	// Transport failure and a bad URL.
	c3 := New(Config{Endpoint: "http://127.0.0.1:1/{q}"})
	if _, err := c3.Search(ctx, "x"); err == nil {
		t.Fatal("expected transport error")
	}
	c4 := New(Config{Endpoint: "http://[::1]:named/{q}"})
	if _, err := c4.Search(ctx, "x"); err == nil {
		t.Fatal("expected url error")
	}
}

func TestSearchAuthorizedSuccess(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		fmt.Fprint(w, `{"results":[{"title":"t","url":"u","snippet":"s","description":"d"}]}`)
	}))
	defer srv.Close()
	c := New(Config{Endpoint: srv.URL + "/s?q={q}", APIKey: "sk-test"})
	res, err := c.Search(context.Background(), "hello world")
	if err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer sk-test" {
		t.Fatalf("auth = %q", auth)
	}
	if len(res) != 1 || res[0].Title != "t" {
		t.Fatalf("results = %+v", res)
	}
}
