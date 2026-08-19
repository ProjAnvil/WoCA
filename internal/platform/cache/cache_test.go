package cache

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestSetGetRoundTrip(t *testing.T) {
	c := NewLocal()
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	key := "k1"
	value := []byte("hello")

	if err := c.Set(ctx, key, value, 0); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, ok, err := c.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !ok {
		t.Fatal("Get: key not found")
	}
	if string(got) != string(value) {
		t.Fatalf("Get = %q, want %q", got, value)
	}
}

func TestGetMissingKey(t *testing.T) {
	c := NewLocal()
	defer func() { _ = c.Close() }()

	got, ok, err := c.Get(context.Background(), "nope")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if ok {
		t.Fatal("Get: ok = true for missing key")
	}
	if got != nil {
		t.Fatalf("Get = %v, want nil", got)
	}
}

func TestDelete(t *testing.T) {
	c := NewLocal()
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	if err := c.Set(ctx, "k", []byte("v"), 0); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := c.Delete(ctx, "k"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok, err := c.Get(ctx, "k"); err != nil || ok {
		t.Fatalf("Get after Delete: ok = %v, err = %v", ok, err)
	}
	// Deleting a missing key must not error.
	if err := c.Delete(ctx, "k"); err != nil {
		t.Fatalf("Delete missing key: %v", err)
	}
}

func TestOverwrite(t *testing.T) {
	c := NewLocal()
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	if err := c.Set(ctx, "k", []byte("old"), 0); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := c.Set(ctx, "k", []byte("new"), 0); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, ok, err := c.Get(ctx, "k")
	if err != nil || !ok {
		t.Fatalf("Get: ok = %v, err = %v", ok, err)
	}
	if string(got) != "new" {
		t.Fatalf("Get = %q, want %q", got, "new")
	}
}

func TestTTLLazyExpiration(t *testing.T) {
	c := NewLocal()
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	if err := c.Set(ctx, "k", []byte("v"), 20*time.Millisecond); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if _, ok, err := c.Get(ctx, "k"); err != nil || !ok {
		t.Fatalf("Get before expiry: ok = %v, err = %v", ok, err)
	}
	time.Sleep(40 * time.Millisecond)
	if _, ok, err := c.Get(ctx, "k"); err != nil || ok {
		t.Fatalf("Get after expiry: ok = %v, err = %v", ok, err)
	}
}

func TestJanitorCleanup(t *testing.T) {
	c := NewLocalWithJanitor(10 * time.Millisecond)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	if err := c.Set(ctx, "k", []byte("v"), 10*time.Millisecond); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := c.Set(ctx, "forever", []byte("v"), 0); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Wait for the janitor to remove the expired entry.
	deadline := time.Now().Add(2 * time.Second)
	for {
		c.mu.Lock()
		n := len(c.items)
		c.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("janitor did not clean expired entry, items = %d", n)
		}
		time.Sleep(5 * time.Millisecond)
	}

	if _, ok, err := c.Get(ctx, "k"); err != nil || ok {
		t.Fatalf("Get expired key: ok = %v, err = %v", ok, err)
	}
	if _, ok, err := c.Get(ctx, "forever"); err != nil || !ok {
		t.Fatalf("Get non-expiring key: ok = %v, err = %v", ok, err)
	}
}

func TestGetReturnsCopy(t *testing.T) {
	c := NewLocal()
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	if err := c.Set(ctx, "k", []byte("abc"), 0); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, ok, err := c.Get(ctx, "k")
	if err != nil || !ok {
		t.Fatalf("Get: ok = %v, err = %v", ok, err)
	}
	got[0] = 'X'

	again, ok, err := c.Get(ctx, "k")
	if err != nil || !ok {
		t.Fatalf("Get: ok = %v, err = %v", ok, err)
	}
	if string(again) != "abc" {
		t.Fatalf("cache mutated through returned slice: %q", again)
	}
}

func TestSetStoresCopy(t *testing.T) {
	c := NewLocal()
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	value := []byte("abc")
	if err := c.Set(ctx, "k", value, 0); err != nil {
		t.Fatalf("Set: %v", err)
	}
	value[0] = 'X'

	got, ok, err := c.Get(ctx, "k")
	if err != nil || !ok {
		t.Fatalf("Get: ok = %v, err = %v", ok, err)
	}
	if string(got) != "abc" {
		t.Fatalf("cache affected by caller mutating input: %q", got)
	}
}

func TestClosed(t *testing.T) {
	c := NewLocal()
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	ctx := context.Background()
	if _, _, err := c.Get(ctx, "k"); !errors.Is(err, ErrClosed) {
		t.Fatalf("Get after Close: err = %v, want ErrClosed", err)
	}
	if err := c.Set(ctx, "k", []byte("v"), 0); !errors.Is(err, ErrClosed) {
		t.Fatalf("Set after Close: err = %v, want ErrClosed", err)
	}
	if err := c.Delete(ctx, "k"); !errors.Is(err, ErrClosed) {
		t.Fatalf("Delete after Close: err = %v, want ErrClosed", err)
	}
	// Close is idempotent.
	if err := c.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestConcurrentAccess(t *testing.T) {
	c := NewLocalWithJanitor(5 * time.Millisecond)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			key := fmt.Sprintf("key-%d", id)
			for j := 0; j < 100; j++ {
				if err := c.Set(ctx, key, []byte{byte(j)}, time.Duration(j%3)*time.Millisecond); err != nil {
					t.Errorf("Set: %v", err)
					return
				}
				if _, _, err := c.Get(ctx, key); err != nil {
					t.Errorf("Get: %v", err)
					return
				}
				if err := c.Delete(ctx, key); err != nil {
					t.Errorf("Delete: %v", err)
					return
				}
			}
		}(i)
	}
	wg.Wait()
}
