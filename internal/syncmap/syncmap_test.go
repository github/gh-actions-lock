package syncmap

import (
	"sync"
	"testing"
)

// TestMapZeroValueUsable ensures Put/Get on a zero-value Map doesn't panic.
func TestMapZeroValueUsable(t *testing.T) {
	var m Map[string, int]
	m.Put("a", 1)
	if v, ok := m.Get("a"); !ok || v != 1 {
		t.Fatalf("zero-value Put/Get failed: got (%d, %v)", v, ok)
	}
}

// TestMapLen covers Len on a zero-value map and after Put, including
// overwrite (same key does not grow the count).
func TestMapLen(t *testing.T) {
	var m Map[string, int]
	if got := m.Len(); got != 0 {
		t.Fatalf("zero-value Len = %d, want 0", got)
	}
	m.Put("a", 1)
	m.Put("b", 2)
	m.Put("a", 3) // overwrite
	if got := m.Len(); got != 2 {
		t.Fatalf("Len = %d, want 2", got)
	}
}

// TestMapConcurrentReadWrite exercises Put and Get from many goroutines so
// `go test -race` flags unsynchronized access.
func TestMapConcurrentReadWrite(t *testing.T) {
	var m Map[int, int]
	const writers, readers, iters = 8, 8, 2000

	var wg sync.WaitGroup
	wg.Add(writers + readers)
	for w := 0; w < writers; w++ {
		go func(base int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				m.Put(base+i, i)
			}
		}(w * iters)
	}
	for r := 0; r < readers; r++ {
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				_, _ = m.Get(i)
			}
		}()
	}
	wg.Wait()
}
