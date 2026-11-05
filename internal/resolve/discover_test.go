package resolve

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/github/gh-actions-lock/internal/dep"
	"github.com/github/gh-actions-lock/internal/ghapi/httpmock"
	"github.com/github/gh-actions-lock/internal/pinpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDiscoverContaining_PrefersHintTag(t *testing.T) {
	// Several tags point at sha; the hint wins and no branch is searched.
	reg := &httpmock.Registry{}
	reg.RegisterReusable(httpmock.REST("GET", `repos/actions/checkout$`),
		httpmock.JSONResponse(map[string]any{"default_branch": "main"}))
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/tags`),
		httpmock.JSONResponse(httpmock.TagListResponse("v4", "abc", "v4.2.1", "abc", "v4.2.2", "abc")),
	)

	r, err := New("github.com", pinpool.New(2, nil), WithTransport(reg))
	if err != nil {
		t.Fatal(err)
	}

	tag, branch, err := r.DiscoverContaining(context.Background(), "actions", "checkout", "abc", "v4.2.2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tag != "v4.2.2" {
		t.Fatalf("expected tag=v4.2.2 (hint match), got %q", tag)
	}
	if branch != "" {
		t.Fatalf("expected no branch search when a tag points at sha, got branch %q", branch)
	}
	reg.Verify(t)
}

func TestDiscoverContaining_BranchOnly(t *testing.T) {
	// sha="def", hintRef="main". main HEAD == "def" → exact match, no compare.
	reg := &httpmock.Registry{}
	httpmock.NoTagsNamed(reg, "actions", "checkout")
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/branches`),
		httpmock.JSONResponse(httpmock.BranchListResponse("main", "def")),
	)
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/tags`),
		httpmock.JSONResponse(httpmock.TagListResponse()),
	)

	r, err := New("github.com", pinpool.New(2, nil), WithTransport(reg))
	if err != nil {
		t.Fatal(err)
	}

	tag, branch, err := r.DiscoverContaining(context.Background(), "actions", "checkout", "def", "main")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tag != "" {
		t.Fatalf("expected empty tag, got %q", tag)
	}
	if branch != "main" {
		t.Fatalf("expected branch=main, got %q", branch)
	}
	reg.Verify(t)
}

func TestDiscoverContaining_NoBranchesFailsClosed(t *testing.T) {
	reg := &httpmock.Registry{}
	// Phase 1: listProtectedBranches (query contains "protected=true") returns empty.
	reg.Register(
		httpmock.RESTWithQuery("GET", `repos/actions/checkout/branches`, "protected=true"),
		httpmock.JSONResponse(httpmock.BranchListResponse()),
	)
	// Phase 2: listBranches (full listing) also returns empty.
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/branches`),
		httpmock.JSONResponse(httpmock.BranchListResponse()),
	)
	// Tags: empty → orphaned commit (no tag either).
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/tags`),
		httpmock.JSONResponse([]any{}),
	)

	r, err := New("github.com", pinpool.New(2, nil), WithTransport(reg))
	if err != nil {
		t.Fatal(err)
	}

	tag, branch, err := r.DiscoverContaining(context.Background(), "actions", "checkout", "dead", "poisoned")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tag != "" || branch != "" {
		t.Fatalf("expected empty tag/branch for orphaned commit, got tag=%q branch=%q", tag, branch)
	}
	reg.Verify(t)
}

func TestDiscoverContainingDefault_PrefersDefaultBranch(t *testing.T) {
	// sha="abc", hintRef="", defaultBranch="main".
	// No exact match → compare path. orderedBranches puts main first.
	reg := &httpmock.Registry{}
	httpmock.NoTagsNamed(reg, "actions", "checkout")
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/branches`),
		httpmock.JSONResponse(httpmock.BranchListResponse("feature/x", "fx000", "main", "m000", "zzz", "z000")),
	)
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/compare/abc`),
		httpmock.JSONResponse(httpmock.CompareAncestorResponse("abc")),
	)
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/tags`),
		httpmock.JSONResponse(httpmock.TagListResponse()),
	)

	r, err := New("github.com", pinpool.New(2, nil), WithTransport(reg))
	if err != nil {
		t.Fatal(err)
	}

	_, branch, err := r.DiscoverContainingDefault(context.Background(), "actions", "checkout", "abc", "", "main")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if branch != "main" {
		t.Fatalf("expected branch=main (default), got %q", branch)
	}
	reg.Verify(t)
}

func TestDiscoverContaining_AutoDiscoversDefaultAndPrefersProtected(t *testing.T) {
	// No hintRef, no exact-match branch. Protected branches are checked in
	// phase 1: main (default) is tried first, then releases/v4. Unprotected
	// branches (aaa, zzz) are not in the protected-branch API response and
	// so are never compared. main doesn't contain abc; releases/v4 does.
	reg := &httpmock.Registry{}
	httpmock.NoTagsNamed(reg, "actions", "checkout")
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout$`),
		httpmock.JSONResponse(map[string]any{"default_branch": "main"}),
	)
	// Phase 1: listProtectedBranches returns only the truly-protected branches.
	reg.Register(
		httpmock.RESTWithQuery("GET", `repos/actions/checkout/branches`, "protected=true"),
		httpmock.JSONResponse(httpmock.BranchListResponseProtected(
			"main", "m000", true,
			"releases/v4", "rv4000", true,
		)),
	)
	// First compare hit: abc...m000 (default branch) — not an ancestor.
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/compare/abc\.\.\.m000`),
		httpmock.JSONResponse(httpmock.CompareAncestorResponse("0000")),
	)
	// Second compare hit: abc...rv4000 (protected) — match.
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/compare/abc\.\.\.rv4000`),
		httpmock.JSONResponse(httpmock.CompareAncestorResponse("abc")),
	)
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/tags`),
		httpmock.JSONResponse(httpmock.TagListResponse()),
	)

	r, err := New("github.com", pinpool.New(2, nil), WithTransport(reg))
	if err != nil {
		t.Fatal(err)
	}

	_, branch, err := r.DiscoverContaining(context.Background(), "actions", "checkout", "abc", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if branch != "releases/v4" {
		t.Fatalf("expected branch=releases/v4 (first protected ancestor), got %q", branch)
	}
	reg.Verify(t)
}

func TestDiscoverContaining_UnprotectedOnlyFallsBack(t *testing.T) {
	// Repo has no protected branches. We fall back to the full branch
	// list.
	reg := &httpmock.Registry{}
	httpmock.NoTagsNamed(reg, "actions", "checkout")
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/branches`),
		httpmock.JSONResponse(httpmock.BranchListResponseProtected(
			"main", "abc", false,
			"dev", "d000", false,
		)),
	)
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/tags`),
		httpmock.JSONResponse(httpmock.TagListResponse()),
	)

	r, err := New("github.com", pinpool.New(2, nil), WithTransport(reg))
	if err != nil {
		t.Fatal(err)
	}

	_, branch, err := r.DiscoverContaining(context.Background(), "actions", "checkout", "abc", "main")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if branch != "main" {
		t.Fatalf("expected branch=main (only candidate), got %q", branch)
	}
	reg.Verify(t)
}

func TestDiscoverContaining_CommitOnlyOnUnprotectedBranchFallsBack(t *testing.T) {
	// main is protected but does NOT contain abc. dev is unprotected and
	// contains abc. Phase 1 scans protected branches only (main); compare
	// shows abc is not on main. Phase 2 full scan finds dev via exact match.
	reg := &httpmock.Registry{}
	httpmock.NoTagsNamed(reg, "actions", "checkout")
	// Phase 1: listProtectedBranches returns only main.
	reg.Register(
		httpmock.RESTWithQuery("GET", `repos/actions/checkout/branches`, "protected=true"),
		httpmock.JSONResponse(httpmock.BranchListResponseProtected(
			"main", "m000", true,
		)),
	)
	// Phase 1 compare: abc not an ancestor of main.
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/compare/abc\.\.\.m000`),
		httpmock.JSONResponse(httpmock.CompareAncestorResponse("0000")),
	)
	// Phase 2: full branch listing includes unprotected dev.
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/branches`),
		httpmock.JSONResponse(httpmock.BranchListResponseProtected(
			"main", "m000", true,
			"dev", "abc", false,
		)),
	)
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/tags`),
		httpmock.JSONResponse(httpmock.TagListResponse()),
	)

	r, err := New("github.com", pinpool.New(2, nil), WithTransport(reg))
	if err != nil {
		t.Fatal(err)
	}

	_, branch, err := r.DiscoverContaining(context.Background(), "actions", "checkout", "abc", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if branch != "dev" {
		t.Fatalf("expected branch=dev (fallback exact-match), got %q", branch)
	}
	reg.Verify(t)
}

func TestReverseLookup_PopulatesTagAndRewritesSHAPins(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.RegisterReusable(httpmock.REST("GET", `repos/actions/checkout$`),
		httpmock.JSONResponse(map[string]any{"default_branch": "main"}))
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/tags`),
		httpmock.JSONResponse(httpmock.TagListResponse("v4", "abc123")),
	)

	r, err := New("github.com", pinpool.New(2, nil), WithTransport(reg))
	if err != nil {
		t.Fatal(err)
	}

	deps := []dep.Dependency{
		{NWO: "actions/checkout", Ref: "abc123abc123abc123abc123abc123abc123abc1", SHA: "abc123", HashAlgo: "sha1"},
	}

	rewrites, _, err := r.ReverseLookup(context.Background(), deps)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := deps[0].Tag; got != "v4" {
		t.Errorf("expected Tag=v4, got %q", got)
	}
	if got := deps[0].Branch; got != "" {
		t.Errorf("expected no Branch when a tag points at the SHA, got %q", got)
	}
	if got := deps[0].Ref; got != "v4" {
		t.Errorf("expected Ref rewritten to v4, got %q", got)
	}
	oldUses := "actions/checkout@abc123abc123abc123abc123abc123abc123abc1"
	if got := rewrites[oldUses]; got != "actions/checkout@v4" {
		t.Errorf("expected rewrite %s → actions/checkout@v4, got %q", oldUses, got)
	}
	reg.Verify(t)
}

func TestReverseLookup_NoChangeWhenRefAlreadyCanonical(t *testing.T) {
	reg := &httpmock.Registry{}

	r, err := New("github.com", pinpool.New(2, nil), WithTransport(reg))
	if err != nil {
		t.Fatal(err)
	}

	deps := []dep.Dependency{
		{NWO: "actions/checkout", Ref: "v4", SHA: "abc", HashAlgo: "sha1"},
	}

	rewrites, _, err := r.ReverseLookup(context.Background(), deps)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rewrites) != 0 {
		t.Errorf("expected no rewrites when ref already canonical, got %v", rewrites)
	}
	if deps[0].Ref != "v4" {
		t.Errorf("ref should be unchanged, got %q", deps[0].Ref)
	}
	if deps[0].Tag != "" || deps[0].Branch != "" {
		t.Errorf("expected Tag and Branch empty, got Tag=%q Branch=%q", deps[0].Tag, deps[0].Branch)
	}
	reg.Verify(t)
}

func TestReverseLookup_FailsClosedOnImpostor(t *testing.T) {
	// DiscoverContaining makes several REST calls (individual branch lookups,
	// default-branch, protected branches, full listing, tags). Use a custom
	// transport that returns empty lists / 404s so the dep reports as an issue.
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if strings.Contains(req.URL.Path, "/git/ref/") {
			return &http.Response{
				StatusCode: 404,
				Body:       io.NopCloser(strings.NewReader(`{"message":"Not Found"}`)),
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Request:    req,
			}, nil
		}
		// repos/:owner/:repo → repo metadata (default_branch)
		if req.URL.Path == "/api/v3/repos/actions/checkout" {
			return &http.Response{
				StatusCode: 200,
				Body:       io.NopCloser(strings.NewReader(`{"default_branch":"main"}`)),
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Request:    req,
			}, nil
		}
		// Everything else (branches listing, tags, etc.) → empty array
		return &http.Response{
			StatusCode: 200,
			Body:       io.NopCloser(strings.NewReader(`[]`)),
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Request:    req,
		}, nil
	})
	r, err := New("github.com", pinpool.New(2, nil), WithTransport(transport))
	if err != nil {
		t.Fatal(err)
	}

	deps := []dep.Dependency{
		{
			NWO: "actions/checkout",
			Ref: "deaddeaddeaddeaddeaddeaddeaddeaddeaddead",
			SHA: "deaddeaddeaddeaddeaddeaddeaddeaddeaddead",
		},
	}
	_, issues, err := r.ReverseLookup(context.Background(), deps)
	if err != nil {
		t.Fatalf("unexpected hard error: %v", err)
	}
	if len(issues) == 0 {
		t.Fatalf("expected a lookup issue for orphaned commit, got none")
	}
	if issues[0].NWO != "actions/checkout" {
		t.Errorf("expected NWO=actions/checkout, got %q", issues[0].NWO)
	}
	want := "this commit has no exact tag and is not on any branch. Update the workflow to use a current upstream tag or branch, then rerun"
	if issues[0].Message != want {
		t.Errorf("message = %q, want %q", issues[0].Message, want)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestReverseLookup_PreservesBranchRefOverTag(t *testing.T) {
	reg := &httpmock.Registry{}

	r, err := New("github.com", pinpool.New(2, nil), WithTransport(reg))
	if err != nil {
		t.Fatal(err)
	}

	deps := []dep.Dependency{
		{NWO: "actions/checkout", Ref: "main", SHA: "abc", HashAlgo: "sha1"},
	}

	rewrites, _, err := r.ReverseLookup(context.Background(), deps)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rewrites) != 0 {
		t.Errorf("expected no rewrites when user wrote a branch ref, got %v", rewrites)
	}
	if deps[0].Ref != "main" {
		t.Errorf("expected Ref=main (preserved), got %q", deps[0].Ref)
	}
	if deps[0].Tag != "" {
		t.Errorf("expected Tag empty, got %q", deps[0].Tag)
	}
	if deps[0].Branch != "" {
		t.Errorf("expected Branch empty, got %q", deps[0].Branch)
	}
	reg.Verify(t)
}

// TestDiscoverContaining_SkipsBranchShadowedByTag: the Actions backend
// resolver looks a recorded ref up as a tag first, so a branch whose name is
// also a tag would resolve to the tag. Such a branch is never recorded,
// even when the tag is beyond the first page of the tag list.
func TestDiscoverContaining_SkipsBranchShadowedByTag(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/branches`),
		httpmock.JSONResponse(httpmock.BranchListResponse("main", "m000", "release", "r000")),
	)
	for range 2 {
		reg.Register(
			httpmock.REST("GET", `repos/actions/checkout/compare/abc`),
			httpmock.JSONResponse(httpmock.CompareAncestorResponse("abc")),
		)
	}
	// The listing's first page doesn't include the tag named main.
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/tags`),
		httpmock.JSONResponse(httpmock.TagListResponse("v1", "zzz")),
	)
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/git/ref/tags/main$`),
		httpmock.JSONResponse(httpmock.GitRefResponse("refs/tags/main", "zzz")),
	)
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/git/ref/tags/release$`),
		httpmock.StatusResponse(http.StatusNotFound),
	)

	r, err := New("github.com", pinpool.New(2, nil), WithTransport(reg))
	if err != nil {
		t.Fatal(err)
	}

	tag, branch, err := r.DiscoverContainingDefault(context.Background(), "actions", "checkout", "abc", "", "main")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tag != "" {
		t.Fatalf("expected no tag at abc, got %q", tag)
	}
	if branch != "release" {
		t.Fatalf("expected the unshadowed branch release, got %q", branch)
	}
}

// TestDiscoverContaining_ShadowCheckFailureFailsClosed: a tag lookup that
// can't finish is never read as "no such tag".
func TestDiscoverContaining_ShadowCheckFailureFailsClosed(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/branches`),
		httpmock.JSONResponse(httpmock.BranchListResponse("main", "abc")),
	)
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/git/ref/tags/main$`),
		httpmock.StatusResponse(http.StatusBadGateway),
	)

	r, err := New("github.com", pinpool.New(2, nil), WithTransport(reg))
	if err != nil {
		t.Fatal(err)
	}

	if _, _, err := r.DiscoverContainingDefault(context.Background(), "actions", "checkout", "abc", "main", "main"); err == nil {
		t.Fatal("expected an error")
	}
}

// TestDiscoverContaining_SkipsRejectedRefNames: a tag or branch name the
// Actions backend doesn't accept is never chosen as the recorded ref.
func TestDiscoverContaining_SkipsRejectedRefNames(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/branches`),
		httpmock.JSONResponse(httpmock.BranchListResponse("fix-gabcd", "abc", "main", "m000")),
	)
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/compare/abc`),
		httpmock.JSONResponse(httpmock.CompareAncestorResponse("abc")),
	)
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/tags`),
		httpmock.JSONResponse(httpmock.TagListResponse("release-g1234", "abc")),
	)
	httpmock.NoTagsNamed(reg, "actions", "checkout")

	r, err := New("github.com", pinpool.New(2, nil), WithTransport(reg))
	if err != nil {
		t.Fatal(err)
	}

	tag, branch, err := r.DiscoverContainingDefault(context.Background(), "actions", "checkout", "abc", "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tag != "" {
		t.Fatalf("expected the rejected tag to be skipped, got %q", tag)
	}
	if branch != "main" {
		t.Fatalf("expected branch main, got %q", branch)
	}
}

// TestDiscoverContaining_RefsOnLaterPages: a commit reachable only through a
// tag or branch past the listings' first pages is placed, not called
// off-history; one no ref contains still is.
func TestDiscoverContaining_RefsOnLaterPages(t *testing.T) {
	page := func(prefix string, n int, sha string) []string {
		var out []string
		for i := range n {
			out = append(out, fmt.Sprintf("%s%03d", prefix, i), sha)
		}
		return out
	}
	tests := []struct {
		name       string
		branches   [][]string
		tagPage2   []string
		wantTag    string
		wantBranch string
		wantNone   bool
	}{
		{name: "tag on page 2", branches: [][]string{page("b", 3, "zzz")}, tagPage2: []string{"twin", "abc"}, wantTag: "twin"},
		{name: "branch on page 4", branches: [][]string{page("a", 100, "zzz"), page("b", 100, "zzz"), page("c", 100, "zzz"), {"late", "abc"}}, wantBranch: "late"},
		{name: "no ref contains it", branches: [][]string{page("b", 3, "zzz")}, wantNone: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := &httpmock.Registry{}
			for i, p := range tt.branches {
				reg.RegisterReusable(httpmock.RESTWithQuery("GET", `repos/o/r/branches`, fmt.Sprintf("&page=%d", i+1)),
					httpmock.JSONResponse(httpmock.BranchListResponse(p...)))
			}
			reg.RegisterReusable(httpmock.REST("GET", `repos/o/r/compare/abc\.\.\.zzz`), httpmock.StatusResponse(http.StatusNotFound))
			reg.RegisterReusable(httpmock.REST("GET", `repos/o/r/compare/abc\.\.\.abc`), httpmock.JSONResponse(httpmock.CompareAncestorResponse("abc")))
			first := httpmock.JSONResponse(httpmock.TagListResponse(page("v", 100, "zzz")...))
			reg.RegisterReusable(httpmock.RESTWithQuery("GET", `repos/o/r/tags`, "&page=2"), httpmock.JSONResponse(httpmock.TagListResponse(tt.tagPage2...)))
			reg.RegisterReusable(httpmock.RESTWithQuery("GET", `repos/o/r/tags`, "per_page=100"), first)
			httpmock.NoTagsNamed(reg, "o", "r")
			reg.RegisterReusable(httpmock.REST("GET", `repos/o/r/git/ref/heads/`), httpmock.StatusResponse(http.StatusNotFound))
			reg.RegisterReusable(httpmock.REST("GET", `repos/o/r/git/matching-refs/`), httpmock.JSONResponse([]any{}))
			reg.RegisterReusable(httpmock.REST("GET", `repos/o/r$`), httpmock.JSONResponse(map[string]any{"default_branch": "main"}))

			r, err := New("github.com", pinpool.New(2, nil), WithTransport(reg))
			require.NoError(t, err)
			tag, branch, err := r.DiscoverContaining(context.Background(), "o", "r", "abc", "abc")
			require.NoError(t, err)
			if tt.wantNone {
				assert.Empty(t, tag)
				assert.Empty(t, branch)
				return
			}
			assert.Equal(t, tt.wantTag, tag)
			assert.Equal(t, tt.wantBranch, branch)
		})
	}
}

// TestDiscoverContaining_NamePolicyBeforePagination: a first-page tag the
// lockfile can't record doesn't stop the search for a recordable one on a
// later page, and branches the lockfile can't record are never chosen.
func TestDiscoverContaining_NamePolicyBeforePagination(t *testing.T) {
	var first []string
	for i := range 99 {
		first = append(first, fmt.Sprintf("v%03d", i), "zzz")
	}
	first = append(first, "release-g1234", "abc")
	reg := &httpmock.Registry{}
	reg.RegisterReusable(httpmock.RESTWithQuery("GET", `repos/o/r/branches`, "&page=1"),
		httpmock.JSONResponse(httpmock.BranchListResponse("refs/x", "abc", strings.Repeat("a", 40), "abc", "main", "zzz")))
	reg.RegisterReusable(httpmock.REST("GET", `repos/o/r/compare/abc\.\.\.zzz`), httpmock.StatusResponse(http.StatusNotFound))
	reg.RegisterReusable(httpmock.RESTWithQuery("GET", `repos/o/r/tags`, "&page=2"), httpmock.JSONResponse(httpmock.TagListResponse("twin", "abc")))
	reg.RegisterReusable(httpmock.RESTWithQuery("GET", `repos/o/r/tags`, "per_page=100"), httpmock.JSONResponse(httpmock.TagListResponse(first...)))
	httpmock.NoTagsNamed(reg, "o", "r")
	reg.RegisterReusable(httpmock.REST("GET", `repos/o/r/git/ref/heads/`), httpmock.StatusResponse(http.StatusNotFound))
	reg.RegisterReusable(httpmock.REST("GET", `repos/o/r/git/matching-refs/`), httpmock.JSONResponse([]any{}))
	reg.RegisterReusable(httpmock.REST("GET", `repos/o/r$`), httpmock.JSONResponse(map[string]any{"default_branch": "main"}))

	r, err := New("github.com", pinpool.New(2, nil), WithTransport(reg))
	require.NoError(t, err)
	tag, branch, err := r.DiscoverContaining(context.Background(), "o", "r", "abc", "abc")
	require.NoError(t, err)
	assert.Equal(t, "twin", tag)
	assert.Empty(t, branch, "neither refs/x nor a SHA-shaped branch can be recorded")
}

// TestRefNamesCommit: repairing a full-SHA `uses:` to a recorded ref
// requires the ref, resolved tag first, to be the commit now. A branch a
// same-named tag shadows names the tag's commit; a non-semver tag counts.
func TestRefNamesCommit(t *testing.T) {
	tests := []struct {
		name string
		tag  string // commit refs/tags/<name> points at, "" if none
		head string // commit refs/heads/<name> points at, "" if none
		want bool
	}{
		{name: "unshadowed branch at the commit", head: "abc", want: true},
		{name: "branch shadowed by a tag", tag: "def", head: "abc", want: false},
		{name: "tag at the commit", tag: "abc", want: true},
		{name: "tag at another commit", tag: "def", want: false},
		{name: "neither", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := &httpmock.Registry{}
			for kind, sha := range map[string]string{"tags": tt.tag, "heads": tt.head} {
				resp := httpmock.StatusResponse(http.StatusNotFound)
				if sha != "" {
					resp = httpmock.JSONResponse(httpmock.GitRefResponse("refs/"+kind+"/z-1", sha))
				}
				reg.RegisterReusable(httpmock.REST("GET", `repos/o/r/git/ref/`+kind+`/z-1$`), resp)
			}
			r, err := New("github.com", pinpool.New(2, nil), WithTransport(reg))
			require.NoError(t, err)
			ok, err := r.RefNamesCommit(context.Background(), "o", "r", "z-1", "abc")
			require.NoError(t, err)
			assert.Equal(t, tt.want, ok)
		})
	}
}

// TestRefNamesCommit_TenantHost: SHA-pin repair on a tenant host looks the
// recorded ref up on the repository's selected host.
func TestRefNamesCommit_TenantHost(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)
	onTenant := func(path string) httpmock.Matcher {
		return func(req *http.Request) bool {
			return req.URL.Host == "api.tenant.ghe.com" && httpmock.REST("GET", path)(req)
		}
	}
	reg.Register(onTenant(`repos/o/r$`), httpmock.JSONResponse(map[string]any{"id": 2, "owner": map[string]any{"id": 1}}))
	reg.Register(onTenant(`repos/o/r/git/ref/tags/z-1$`), httpmock.JSONResponse(httpmock.GitRefResponse("refs/tags/z-1", "abc")))
	r, err := New("tenant.ghe.com", pinpool.New(2, nil), WithTransport(reg))
	require.NoError(t, err)
	ok, err := r.RefNamesCommit(context.Background(), "o", "r", "z-1", "abc")
	require.NoError(t, err)
	assert.True(t, ok)
}
