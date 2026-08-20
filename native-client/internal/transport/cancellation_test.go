package transport

import (
	"context"
	"strings"
	"testing"
	"time"
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
	defer unregister()
	if ok {
		t.Fatal("register() admitted a request cancelled before registration")
	}
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

func TestCancellationRegistryOverflowFailsClosedAndRecovers(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)
	now := start
	registry := newCancellationRegistryWithClock(func() time.Time { return now })
	for i := 0; i < maxPendingCancellations; i++ {
		id := indexedRequestID(i)
		if !registry.Cancel(id) {
			t.Fatalf("Cancel() rejected pending ID %d before capacity", i)
		}
	}
	if !registry.Cancel(requestID("D")) {
		t.Fatal("Cancel() did not enter fail-closed overflow state")
	}
	lastCancel := start.Add(pendingCancellationTTL - time.Nanosecond)
	now = lastCancel
	if !registry.Cancel(requestID("E")) {
		t.Fatal("Cancel() rejected a valid ID during overflow")
	}
	now = start.Add(pendingCancellationTTL + time.Nanosecond)
	for _, id := range []string{indexedRequestID(0), requestID("D"), requestID("E")} {
		ctx, unregister, ok := registry.register(context.Background(), id)
		unregister()
		if ok {
			t.Fatalf("register(%q) was admitted during overflow", id)
		}
		select {
		case <-ctx.Done():
		default:
			t.Fatalf("register(%q) did not fail with a cancelled context", id)
		}
	}

	now = lastCancel.Add(pendingCancellationTTL - time.Nanosecond)
	ctx, unregister, ok := registry.register(context.Background(), requestID("E"))
	unregister()
	if ok {
		t.Fatal("registry recovered before a full TTL from the last accepted cancellation")
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("pre-deadline registration was not rejected with a cancelled context")
	}

	now = lastCancel.Add(pendingCancellationTTL)
	ctx, unregister, ok = registry.register(context.Background(), requestID("E"))
	defer unregister()
	if !ok {
		t.Fatal("registry did not recover after the bounded overflow window")
	}
	select {
	case <-ctx.Done():
		t.Fatal("recovered registration inherited an expired cancellation")
	default:
	}
}

func TestCancellationRegistryClockRegressionCannotShortenPendingOrOverflow(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)
	now := start.Add(10 * time.Second)
	pendingRegistry := newCancellationRegistryWithClock(func() time.Time { return now })
	id := requestID("J")
	if !pendingRegistry.Cancel(id) {
		t.Fatal("initial pending cancellation failed")
	}
	now = start
	if !pendingRegistry.Cancel(id) {
		t.Fatal("repeated pending cancellation failed after clock regression")
	}
	now = start.Add(30 * time.Second)
	ctx, unregister, ok := pendingRegistry.register(context.Background(), id)
	unregister()
	if ok {
		t.Fatal("regressed clock shortened a pending cancellation deadline")
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("pending registration was not rejected with a cancelled context")
	}

	now = start.Add(10 * time.Second)
	overflowRegistry := newCancellationRegistryWithClock(func() time.Time { return now })
	for index := 0; index < maxPendingCancellations; index++ {
		if !overflowRegistry.Cancel(indexedRequestID(index)) {
			t.Fatalf("Cancel() rejected pending ID %d", index)
		}
	}
	if !overflowRegistry.Cancel(requestID("K")) {
		t.Fatal("initial overflow cancellation failed")
	}
	now = start
	if !overflowRegistry.Cancel(requestID("L")) {
		t.Fatal("overflow cancellation failed after clock regression")
	}
	now = start.Add(30 * time.Second)
	ctx, unregister, ok = overflowRegistry.register(context.Background(), requestID("M"))
	unregister()
	if ok {
		t.Fatal("regressed clock shortened the overflow deadline")
	}
}

func TestCancellationRegistrySamplesClockUnderMutex(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	clockObservedUnlocked := false
	var registry *CancellationRegistry
	registry = newCancellationRegistryWithClock(func() time.Time {
		if registry.mu.TryLock() {
			clockObservedUnlocked = true
			registry.mu.Unlock()
		}
		return now
	})

	if !registry.Cancel(requestID("N")) {
		t.Fatal("Cancel() rejected valid ID")
	}
	if clockObservedUnlocked {
		t.Fatal("registry sampled its clock outside the mutex")
	}
}

func TestCancellationRegistryExpiresUnmatchedPendingCancellation(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	registry := newCancellationRegistryWithClock(func() time.Time { return now })
	id := requestID("F")
	if !registry.Cancel(id) {
		t.Fatal("Cancel() rejected pending ID")
	}
	now = now.Add(pendingCancellationTTL + time.Nanosecond)
	ctx, unregister, ok := registry.register(context.Background(), id)
	defer unregister()
	if !ok {
		t.Fatal("expired pending cancellation permanently poisoned the ID")
	}
	select {
	case <-ctx.Done():
		t.Fatal("expired pending cancellation cancelled a later request")
	default:
	}
}

func TestCancellationRegistryCancelAllCloses(t *testing.T) {
	registry := NewCancellationRegistry()
	activeID := requestID("G")
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
	if registry.Cancel(requestID("H")) {
		t.Fatal("Cancel() accepted work after CancelAll()")
	}
	if _, _, ok := registry.register(context.Background(), requestID("I")); ok {
		t.Fatal("register() accepted work after CancelAll()")
	}
}

func TestCancellationRegistryCancelAllDoesNotConsultClock(t *testing.T) {
	registry := newCancellationRegistryWithClock(func() time.Time {
		panic("clock read after CancelAll")
	})
	registry.CancelAll()
	if registry.Cancel(requestID("O")) {
		t.Fatal("Cancel() accepted work after CancelAll()")
	}
	if _, _, ok := registry.register(context.Background(), requestID("P")); ok {
		t.Fatal("register() accepted work after CancelAll()")
	}
}

func indexedRequestID(index int) string {
	alphabet := "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	return strings.Repeat("A", 41) + string(alphabet[(index/64)%64]) + string(alphabet[index%64])
}
