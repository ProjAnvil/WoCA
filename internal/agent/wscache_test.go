package agent

import "testing"

func TestWSCacheEvictsOldest(t *testing.T) {
	c := newWSCache[string](2)
	c.Set("a", "1")
	c.Set("b", "2")
	c.Get("a")      // a becomes hot
	c.Set("c", "3") // evicts b
	if _, ok := c.Get("b"); ok {
		t.Fatal("b should have been evicted")
	}
	if v, ok := c.Get("a"); !ok || v != "1" {
		t.Fatalf("a = %q,%v; want 1,true", v, ok)
	}
	if v, ok := c.Get("c"); !ok || v != "3" {
		t.Fatalf("c = %q,%v; want 3,true", v, ok)
	}
}

func TestWSCacheClear(t *testing.T) {
	c := newWSCache[int](32)
	c.Set("x", 1)
	c.Clear()
	if _, ok := c.Get("x"); ok {
		t.Fatal("Get after Clear should miss")
	}
}
