package ghapi

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/github/gh-actions-pin/internal/ghapi/httpmock"
)

// countingTransport is a RoundTripper that records how many times each URL
// path is requested and serves a canned response per endpoint shape. Unlike
// httpmock.Registry (whose stubs match once), it answers repeated requests so
// a missing-singleflight stampede shows up as a count > 1.
type countingTransport struct {
	mu     sync.Mutex
	counts map[string]int
	delay  time.Duration // widen the cache-miss window so concurrent callers overlap
}

func newCountingTransport(delay time.Duration) *countingTransport {
	return &countingTransport{counts: map[string]int{}, delay: delay}
}

func (t *countingTransport) count(substr string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	total := 0
	for p, n := range t.counts {
		if strings.Contains(p, substr) {
			total += n
		}
	}
	return total
}

func (t *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.mu.Lock()
	t.counts[req.URL.Path]++
	t.mu.Unlock()

	if t.delay > 0 {
		time.Sleep(t.delay)
	}

	p := req.URL.Path
	switch {
	case strings.Contains(p, "/compare/"):
		return httpmock.JSONResponse(map[string]any{
			"status":            "behind",
			"merge_base_commit": map[string]string{"sha": "old"},
		})(req)
	case strings.Contains(p, "/git/ref/heads/"):
		return httpmock.JSONResponse(map[string]any{
			"ref":    "refs/heads/main",
			"object": map[string]string{"sha": "headsha", "type": "commit"},
		})(req)
	default: // repos/{owner}/{repo}
		return httpmock.JSONResponse(map[string]any{
			"default_branch": "main",
			"id":             int64(20),
			"owner":          map[string]any{"id": int64(10)},
		})(req)
	}
}

func newCountingClient(t *testing.T, tr http.RoundTripper) *Client {
	t.Helper()
	c, err := New("github.com", WithClientTransport(tr))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// runConcurrent fans out fn across n goroutines that start together.
func runConcurrent(n int, fn func()) {
	var start sync.WaitGroup
	var done sync.WaitGroup
	start.Add(1)
	done.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer done.Done()
			start.Wait()
			fn()
		}()
	}
	start.Done()
	done.Wait()
}

func TestCompareCommits_CoalescesConcurrent(t *testing.T) {
	tr := newCountingTransport(2 * time.Millisecond)
	c := newCountingClient(t, tr)

	runConcurrent(50, func() {
		got, err := c.CompareCommits(context.Background(), "o", "r", "old", "new")
		if err != nil || !got {
			t.Errorf("CompareCommits = (%v, %v), want (true, nil)", got, err)
		}
	})

	if n := tr.count("/compare/"); n != 1 {
		t.Fatalf("expected 1 compare request, got %d", n)
	}
}

func TestGetBranchHead_CoalescesConcurrent(t *testing.T) {
	tr := newCountingTransport(2 * time.Millisecond)
	c := newCountingClient(t, tr)

	runConcurrent(50, func() {
		bh, ok := c.GetBranchHead(context.Background(), "o", "r", "main")
		if !ok || bh.SHA != "headsha" {
			t.Errorf("GetBranchHead = (%+v, %v), want sha=headsha ok=true", bh, ok)
		}
	})

	if n := tr.count("/git/ref/heads/"); n != 1 {
		t.Fatalf("expected 1 git/ref request, got %d", n)
	}
}

func TestRepoIDs_CoalescesConcurrent(t *testing.T) {
	tr := newCountingTransport(2 * time.Millisecond)
	c := newCountingClient(t, tr)

	runConcurrent(50, func() {
		owner, repo, err := c.RepoIDs(context.Background(), "o", "r")
		if err != nil || owner != 10 || repo != 20 {
			t.Errorf("RepoIDs = (%d, %d, %v), want (10, 20, nil)", owner, repo, err)
		}
	})

	if n := tr.count("repos/o/r"); n != 1 {
		t.Fatalf("expected 1 repo request, got %d", n)
	}
}

// RepoIDs and GetDefaultBranch both derive from repos/{owner}/{repo}; they
// must share a single round-trip rather than fetching it twice.
func TestRepoMetadata_SharedAcrossConsumers(t *testing.T) {
	tr := newCountingTransport(2 * time.Millisecond)
	c := newCountingClient(t, tr)

	runConcurrent(50, func() {
		if got := c.GetDefaultBranch(context.Background(), "o", "r"); got != "main" {
			t.Errorf("GetDefaultBranch = %q, want main", got)
		}
		if owner, repo, err := c.RepoIDs(context.Background(), "o", "r"); err != nil || owner != 10 || repo != 20 {
			t.Errorf("RepoIDs = (%d, %d, %v)", owner, repo, err)
		}
	})

	if n := tr.count("repos/o/r"); n != 1 {
		t.Fatalf("expected 1 shared repo request, got %d", n)
	}
}

// A coalesced caller fans out under a scan/errgroup context that cancels on
// first match or error. The shared request must not inherit that cancellation,
// otherwise one caller's cancel would poison the result for everyone waiting on
// it. These assert a pre-canceled caller still gets the real answer.

func TestCompareCommits_IgnoresCallerCancel(t *testing.T) {
	tr := newCountingTransport(0)
	c := newCountingClient(t, tr)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got, err := c.CompareCommits(ctx, "o", "r", "old", "new")
	if err != nil || !got {
		t.Fatalf("CompareCommits with canceled ctx = (%v, %v), want (true, nil)", got, err)
	}
}

func TestGetBranchHead_IgnoresCallerCancel(t *testing.T) {
	tr := newCountingTransport(0)
	c := newCountingClient(t, tr)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	bh, ok := c.GetBranchHead(ctx, "o", "r", "main")
	if !ok || bh.SHA != "headsha" {
		t.Fatalf("GetBranchHead with canceled ctx = (%+v, %v), want sha=headsha ok=true", bh, ok)
	}
}

func TestRepoIDs_IgnoresCallerCancel(t *testing.T) {
	tr := newCountingTransport(0)
	c := newCountingClient(t, tr)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	owner, repo, err := c.RepoIDs(ctx, "o", "r")
	if err != nil || owner != 10 || repo != 20 {
		t.Fatalf("RepoIDs with canceled ctx = (%d, %d, %v), want (10, 20, nil)", owner, repo, err)
	}
}
