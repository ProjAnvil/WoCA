package transport

import (
	"context"
	"strings"
	"testing"
)

func requestID(fill string) string {
	return strings.Repeat(fill, 43)
}

func TestCancellationRegistryCancelBeforeRegister(t *testing.T) {
	registry := NewCancellationRegistry()
	id := requestID("A")

	if !registry.Cancel(id) {
		t.Fatal("Cancel() did not retain a valid pre-registration cancellation")
	}
	ctx, unregister, ok := registry.register(context.Background(), id)
	if !ok {
		t.Fatal("register() rejected a valid pending request ID")
	}
	defer unregister()
	select {
	case <-ctx.Done():
	default:
		t.Fatal("pending cancellation did not cancel the registered context")
	}
}

func TestCancellationRegistryRejectsInvalidAndDuplicateIDs(t *testing.T) {
	registry := NewCancellationRegistry()
	for _, id := range []string{
		"",
		requestID("A")[:42],
		requestID("A") + "A",
		strings.Repeat("+", 43),
		strings.Repeat("/", 43),
		strings.Repeat("=", 43),
		strings.Repeat(" ", 43),
	} {
		if registry.Cancel(id) {
			t.Fatalf("Cancel(%q) accepted an invalid request ID", id)
		}
		if _, _, ok := registry.register(context.Background(), id); ok {
			t.Fatalf("register(%q) accepted an invalid request ID", id)
		}
	}

	id := requestID("B")
	first, unregister, ok := registry.register(context.Background(), id)
	if !ok {
		t.Fatal("first registration failed")
	}
	defer unregister()
	if _, _, ok := registry.register(context.Background(), id); ok {
		t.Fatal("duplicate active request ID was accepted")
	}
	select {
	case <-first.Done():
		t.Fatal("duplicate registration cancelled the original request")
	default:
	}
}

func TestCancellationRegistryUnregisterIsGenerationSafe(t *testing.T) {
	registry := NewCancellationRegistry()
	id := requestID("C")

	_, unregisterFirst, ok := registry.register(context.Background(), id)
	if !ok {
		t.Fatal("first registration failed")
	}
	unregisterFirst()
	second, unregisterSecond, ok := registry.register(context.Background(), id)
	if !ok {
		t.Fatal("second registration failed after natural unregister")
	}
	defer unregisterSecond()

	unregisterFirst()
	if !registry.Cancel(id) {
		t.Fatal("Cancel() did not find the current generation")
	}
	select {
	case <-second.Done():
	default:
		t.Fatal("stale unregister removed the current generation")
	}
}

func TestCancellationRegistryBoundsPendingAndCancelAllCloses(t *testing.T) {
	registry := NewCancellationRegistry()
	for i := 0; i < maxPendingCancellations; i++ {
		alphabet := "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
		id := strings.Repeat("A", 41) + string(alphabet[(i/64)%64]) + string(alphabet[i%64])
		if !registry.Cancel(id) {
			t.Fatalf("Cancel() rejected pending ID %d before capacity", i)
		}
	}
	if registry.Cancel(requestID("D")) {
		t.Fatal("Cancel() admitted an unbounded pending cancellation")
	}

	activeID := requestID("E")
	ctx, unregister, ok := registry.register(context.Background(), activeID)
	if !ok {
		t.Fatal("active registration failed")
	}
	defer unregister()
	registry.CancelAll()
	select {
	case <-ctx.Done():
	default:
		t.Fatal("CancelAll() did not cancel an active request")
	}
	if registry.Cancel(requestID("F")) {
		t.Fatal("Cancel() accepted work after CancelAll()")
	}
	if _, _, ok := registry.register(context.Background(), requestID("G")); ok {
		t.Fatal("register() accepted work after CancelAll()")
	}
}
