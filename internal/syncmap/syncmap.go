// Package syncmap provides a simple generic mutex-guarded map. The zero
// value is usable; the underlying map is allocated on first Put.
package syncmap

import "sync"

// Map is a lock-paired map keyed by K. Each instance guards a single map
// with a single mutex — no sharing, no sharding.
type Map[K comparable, V any] struct {
	mu sync.Mutex
	m  map[K]V
}

// Get returns the value for k and whether it was found.
func (c *Map[K, V]) Get(k K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.m[k]
	return v, ok
}

// Put stores v under k, allocating the map on first call.
func (c *Map[K, V]) Put(k K, v V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = make(map[K]V)
	}
	c.m[k] = v
}

// Len returns the number of entries currently stored.
func (c *Map[K, V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.m)
}
