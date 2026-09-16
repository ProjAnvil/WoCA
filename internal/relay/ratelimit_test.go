package relay

import (
	"fmt"
	"testing"
	"time"
)

func TestRateLimiterWindow(t *testing.T) {
	now := time.Now()
	l := NewRateLimiter(10, 100)
	l.now = func() time.Time { return now }

	for i := 0; i < 10; i++ {
		if !l.Allow("1.2.3.4") {
			t.Fatalf("attempt %d denied inside the 10/min window", i+1)
		}
	}
	if l.Allow("1.2.3.4") {
		t.Fatal("11th attempt inside the window allowed")
	}
	// Buckets are per key: another IP has its own allowance.
	if !l.Allow("5.6.7.8") {
		t.Fatal("per-IP isolation broken")
	}
	now = now.Add(61 * time.Second)
	if !l.Allow("1.2.3.4") {
		t.Fatal("window did not refill")
	}
}

func TestRateLimiterBounded(t *testing.T) {
	l := NewRateLimiter(10, 50)
	// A spray of distinct source IPs must not grow the table without
	// bound (memory-DoS posture, design §4.4).
	for i := 0; i < 500; i++ {
		l.Allow(fmt.Sprintf("10.0.%d.%d", i/256, i%256))
	}
	l.mu.Lock()
	n := len(l.entries)
	l.mu.Unlock()
	if n > 50 {
		t.Fatalf("entries %d exceed maxEntries 50", n)
	}
}
