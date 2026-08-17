package agent

import (
	"container/list"
	"sync"
)

// wsCacheCapacity bounds each per-workspace cache (prompts/skills/agents/
// hooks were unbounded sync.Maps before).
const wsCacheCapacity = 32

// wsCache is a tiny mutex-guarded LRU keyed by workspace.
type wsCache[V any] struct {
	mu    sync.Mutex
	cap   int
	ll    *list.List // front = most recently used
	items map[string]*list.Element
}

type wsCacheEntry[V any] struct {
	key string
	val V
}

func newWSCache[V any](capacity int) *wsCache[V] {
	return &wsCache[V]{cap: capacity, ll: list.New(), items: map[string]*list.Element{}}
}

func (c *wsCache[V]) Get(key string) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		c.ll.MoveToFront(el)
		return el.Value.(wsCacheEntry[V]).val, true
	}
	var zero V
	return zero, false
}

func (c *wsCache[V]) Set(key string, val V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		el.Value = wsCacheEntry[V]{key, val}
		c.ll.MoveToFront(el)
		return
	}
	c.items[key] = c.ll.PushFront(wsCacheEntry[V]{key, val})
	for c.ll.Len() > c.cap {
		back := c.ll.Back()
		delete(c.items, back.Value.(wsCacheEntry[V]).key)
		c.ll.Remove(back)
	}
}

// Clear drops every entry (workspace-prompt invalidation semantics).
func (c *wsCache[V]) Clear() {
	c.mu.Lock()
	c.ll.Init()
	c.items = map[string]*list.Element{}
	c.mu.Unlock()
}
