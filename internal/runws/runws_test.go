package runws

import (
	"context"
	"testing"
)

func TestWithFrom(t *testing.T) {
	ctx := With(context.Background(), "/ws/ladyM")
	ws, ok := From(ctx)
	if !ok || ws != "/ws/ladyM" {
		t.Fatalf("From = %q, %v; want /ws/ladyM, true", ws, ok)
	}
}

func TestEmptyIsNoOp(t *testing.T) {
	base := context.Background()
	if got := With(base, ""); got != base {
		t.Fatal("With(\"\") must return the ctx unchanged")
	}
	if _, ok := From(base); ok {
		t.Fatal("From on undecorated ctx must report false")
	}
}

func TestNilContext(t *testing.T) {
	if ws, ok := From(nil); ok || ws != "" {
		t.Fatalf("From(nil) = %q, %v; want \"\", false", ws, ok)
	}
}
