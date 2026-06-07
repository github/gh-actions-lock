package tag

import (
	"context"
	"testing"

	"github.com/github/gh-actions-pin/internal/ghapi/httpmock"
)

// TestTagLister_CaseInsensitiveCacheKeys locks in the discipline that all
// Lister in-memory caches (cache, repoCache, releaseDates) are keyed by
// cachekey.ForRepo, which lowercases owner and repo. Two callers naming the
// same repo with different case must share one cache slot — otherwise we
// would issue duplicate API requests and the case variants would drift.
//
// The cache key type (ghapi.Repo) does not by itself lowercase its fields,
// so constructing a key without ForRepo would reintroduce the collision.
// This test guards against that regression by driving the public API with
// two case variants and asserting only one cache entry results.
//
// Mechanism: register HTTP stubs with case-insensitive regexes so they
// match either case variant. Each stub is single-use; if the second call
// is not a cache hit it will re-issue the request and fail with
// "no registered HTTP stubs matched".
func TestTagLister_CaseInsensitiveCacheKeys(t *testing.T) {
	reg := &httpmock.Registry{}
	// (?i) so the stub matches whichever case the first caller sent. Each
	// stub fires exactly once; a second call that isn't served from cache
	// would error out with no-stubs-matched.
	reg.Register(
		httpmock.REST("GET", `(?i)repos/octocat/hello-world/tags`),
		httpmock.JSONResponse([]map[string]any{
			{"name": "v1.0.0", "commit": map[string]any{"sha": "abc123"}},
		}),
	)
	reg.Register(
		httpmock.REST("GET", `(?i)repos/octocat/hello-world/git/matching-refs/tags`),
		httpmock.JSONResponse([]map[string]any{}),
	)
	reg.Register(
		httpmock.REST("GET", `(?i)repos/octocat/hello-world/releases`),
		httpmock.JSONResponse([]map[string]any{
			{"tag_name": "v1.0.0", "published_at": "2024-01-01T00:00:00Z", "immutable": false},
		}),
	)
	reg.Register(
		// repos/<owner>/<repo> with no trailing path segment.
		httpmock.REST("GET", `(?i)repos/octocat/hello-world$`),
		httpmock.JSONResponse(map[string]any{
			"default_branch": "main",
			"visibility":     "public",
			"pushed_at":      "2024-01-01T00:00:00Z",
		}),
	)

	tl := NewListerForTest(t, reg)

	// First pass: canonical lowercase.
	if _, err := tl.ListTags(context.Background(), "octocat", "hello-world"); err != nil {
		t.Fatalf("ListTags lowercase: %v", err)
	}
	if _, err := tl.GetRepoInfo(context.Background(), "octocat", "hello-world"); err != nil {
		t.Fatalf("GetRepoInfo lowercase: %v", err)
	}
	if got := tl.ReleaseDate("octocat", "hello-world", "v1.0.0"); got == "" {
		t.Fatalf("ReleaseDate lowercase: expected populated date, got empty")
	}

	// Second pass: mixed case for the same repo. Every cache must hit.
	// If any of these fall through to the network, the registry will
	// fail with "no registered HTTP stubs matched" because each stub
	// has already fired.
	if _, err := tl.ListTags(context.Background(), "Octocat", "Hello-World"); err != nil {
		t.Fatalf("ListTags mixed case: %v", err)
	}
	if _, err := tl.GetRepoInfo(context.Background(), "OCTOCAT", "HELLO-WORLD"); err != nil {
		t.Fatalf("GetRepoInfo upper case: %v", err)
	}
	if got := tl.ReleaseDate("Octocat", "Hello-World", "v1.0.0"); got == "" {
		t.Fatalf("ReleaseDate mixed case: expected cache hit, got empty")
	}

	// Belt and suspenders: each cache must hold exactly one entry. A
	// direct map index with un-normalized input would compile silently
	// and reintroduce the collision class cachekey.ForRepo prevents.
	gotTags := tl.tagCache.Len()
	gotRepo := tl.repoCache.Len()
	gotDates := tl.releaseDates.Len()

	if gotTags != 1 {
		t.Errorf("tags cache: expected 1 entry, got %d", gotTags)
	}
	if gotRepo != 1 {
		t.Errorf("repo cache: expected 1 entry, got %d", gotRepo)
	}
	if gotDates != 1 {
		t.Errorf("releaseDates cache: expected 1 entry, got %d", gotDates)
	}

	reg.Verify(t)
}
