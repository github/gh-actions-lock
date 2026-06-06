package resolve

import (
	"sync"
	"testing"
)

// TestSyncMapZeroValueUsable ensures put/get on a zero-value syncMap doesn't
// panic. Tests construct &Resolver{} composite literals without initializing
// every cache field, so the zero value must be safe.
func TestSyncMapZeroValueUsable(t *testing.T) {
	var m syncMap[string, int]
	m.put("a", 1)
	if v, ok := m.get("a"); !ok || v != 1 {
		t.Fatalf("zero-value put/get failed: got (%d, %v)", v, ok)
	}
}

// TestSyncMapConcurrentReadWrite exercises put and get from many goroutines
// at once so `go test -race` flags any unsynchronized map access. This is the
// invariant the per-cache lock refactor is meant to preserve: every read and
// every write goes through one mutex per map, never two.
func TestSyncMapConcurrentReadWrite(t *testing.T) {
	var m syncMap[int, int]
	const writers, readers, iters = 8, 8, 2000

	var wg sync.WaitGroup
	wg.Add(writers + readers)
	for w := 0; w < writers; w++ {
		go func(base int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				m.put(base+i, i)
			}
		}(w * iters)
	}
	for r := 0; r < readers; r++ {
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				_, _ = m.get(i)
			}
		}()
	}
	wg.Wait()
}
