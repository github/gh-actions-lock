package resolve

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/github/gh-actions-lock/internal/dep"
	"github.com/github/gh-actions-lock/internal/ghapi"
	"github.com/github/gh-actions-lock/internal/ghapi/httpmock"
	"github.com/github/gh-actions-lock/internal/pinpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReverseLookup_ContainingBranchDoesNotIdentifyHistoricalCommit(t *testing.T) {
	const (
		oldSHA  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		headSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)
	for _, tt := range []struct {
		name    string
		sha     string
		tag     string
		wantRef string
	}{
		{name: "historical SHA stays distinct", sha: oldSHA, wantRef: oldSHA},
		{name: "branch head can use branch name", sha: headSHA, wantRef: "main"},
		{name: "exact tag identifies historical commit", sha: oldSHA, tag: "v1.0.0", wantRef: "v1.0.0"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			reg := &httpmock.Registry{}
			httpmock.NoTagsNamed(reg, "owner", "action")
			defer reg.Verify(t)
			reg.Register(httpmock.REST("GET", `repos/owner/action$`),
				httpmock.JSONResponse(map[string]any{"default_branch": "main"}))
			if tt.tag == "" {
				reg.Register(httpmock.REST("GET", `repos/owner/action/git/ref/heads/main`),
					httpmock.JSONResponse(map[string]any{
						"ref": "refs/heads/main", "object": map[string]any{"sha": headSHA, "type": "commit"},
					}))
			}
			if tt.tag == "" && tt.sha != headSHA {
				reg.Register(httpmock.REST("GET", `repos/owner/action/compare/`+oldSHA+`\.\.\.`+headSHA),
					httpmock.JSONResponse(httpmock.CompareAncestorResponse(oldSHA)))
			}
			tags := httpmock.TagListResponse()
			if tt.tag != "" {
				tags = httpmock.TagListResponse(tt.tag, tt.sha)
			}
			reg.Register(httpmock.REST("GET", `repos/owner/action/tags`), httpmock.JSONResponse(tags))
			resolver, err := New("github.com", pinpool.New(2, nil), WithTransport(reg))
			require.NoError(t, err)
			deps := []dep.Dependency{{NWO: "owner/action", Ref: tt.sha, SHA: tt.sha}}

			rewrites, issues, err := resolver.ReverseLookup(context.Background(), deps)
			require.NoError(t, err)
			assert.Empty(t, issues)
			assert.Equal(t, tt.wantRef, deps[0].Ref)
			wantBranch := "main"
			if tt.tag != "" {
				wantBranch = ""
			}
			assert.Equal(t, wantBranch, deps[0].Branch)
			assert.Equal(t, tt.tag, deps[0].Tag)
			if tt.wantRef == tt.sha {
				assert.Empty(t, rewrites)
			} else {
				assert.Equal(t, map[string]string{"owner/action@" + tt.sha: "owner/action@" + tt.wantRef}, rewrites)
			}
		})
	}
}

// TestDiscoverContaining_TaggedSHASkipsBranchSearch is #267: in a repo
// where every branch is protected, a tagged SHA must not trigger the
// one-compare-per-branch search, wherever the tag sits in the listing.
func TestDiscoverContaining_TaggedSHASkipsBranchSearch(t *testing.T) {
	const sha = "0daab03d71ff584ef619d027a3fd9146679c5d84"
	filler := func(prefix string, n int) []string {
		var out []string
		for i := range n {
			out = append(out, fmt.Sprintf("%s%03d", prefix, i), "zzz")
		}
		return out
	}
	tests := []struct {
		name      string
		firstPage []string
		page2     []string
	}{
		{name: "tag on first page", firstPage: append(filler("v2.", 99), "v3.35.3", sha)},
		{name: "tag on later page", firstPage: filler("v2.", 100), page2: []string{"v3.35.3", sha}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := &httpmock.Registry{}
			defer reg.Verify(t)
			reg.RegisterReusable(httpmock.REST("GET", `repos/github/codeql-action$`),
				httpmock.JSONResponse(map[string]any{"default_branch": "main"}))
			reg.RegisterReusable(httpmock.RESTWithQuery("GET", `repos/github/codeql-action/tags`, "&page=2"),
				httpmock.JSONResponse(httpmock.TagListResponse(tt.page2...)))
			reg.RegisterReusable(httpmock.RESTWithQuery("GET", `repos/github/codeql-action/tags`, "per_page=100"),
				httpmock.JSONResponse(httpmock.TagListResponse(tt.firstPage...)))
			branchSearch := func(req *http.Request) (*http.Response, error) {
				t.Errorf("unexpected branch search request: %s", req.URL)
				return nil, fmt.Errorf("unexpected request")
			}
			for _, path := range []string{`/compare/`, `/branches`, `/git/ref/heads/`, `/git/matching-refs/`} {
				reg.RegisterReusable(httpmock.REST("GET", `repos/github/codeql-action`+path), branchSearch)
			}

			r, err := New("github.com", pinpool.New(2, nil), WithTransport(reg))
			require.NoError(t, err)
			tag, branch, err := r.DiscoverContaining(context.Background(), "github", "codeql-action", sha, sha)
			require.NoError(t, err)
			assert.Equal(t, "v3.35.3", tag)
			assert.Empty(t, branch)
		})
	}
}

func TestFindContainingBranch_ExactHeadMatch(t *testing.T) {
	reg := &httpmock.Registry{}
	r, err := New("test.com", pinpool.New(1, nil), WithTransport(reg))
	if err != nil {
		t.Fatal(err)
	}
	candidates := []ghapi.BranchHead{
		{Name: "feature", SHA: "other"},
		{Name: "main", SHA: "abc123"},
		{Name: "develop", SHA: "abc123"},
	}

	got, err := r.findContainingBranch(context.Background(), "o", "r", "abc123", "", "main", candidates)
	if err != nil {
		t.Fatal(err)
	}
	// main is default → preferred among exact matches.
	if got != "main" {
		t.Fatalf("expected main (default), got %q", got)
	}
}

func TestFindContainingBranch_HintWinsOverDefault(t *testing.T) {
	reg := &httpmock.Registry{}
	r, err := New("test.com", pinpool.New(1, nil), WithTransport(reg))
	if err != nil {
		t.Fatal(err)
	}
	candidates := []ghapi.BranchHead{
		{Name: "main", SHA: "abc123"},
		{Name: "v2", SHA: "abc123"},
	}

	got, err := r.findContainingBranch(context.Background(), "o", "r", "abc123", "v2", "main", candidates)
	if err != nil {
		t.Fatal(err)
	}
	if got != "v2" {
		t.Fatalf("expected v2 (hint), got %q", got)
	}
}

func TestFindContainingBranch_EmptyCandidates(t *testing.T) {
	reg := &httpmock.Registry{}
	r, err := New("test.com", pinpool.New(1, nil), WithTransport(reg))
	if err != nil {
		t.Fatal(err)
	}

	got, err := r.findContainingBranch(context.Background(), "o", "r", "abc123", "", "main", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Fatalf("expected empty for nil candidates, got %q", got)
	}
}

func TestReverseLookup_SkipsEmptyOwnerRepo(t *testing.T) {
	reg := &httpmock.Registry{}
	r, err := New("test.com", pinpool.New(1, nil), WithTransport(reg))
	if err != nil {
		t.Fatal(err)
	}

	deps := []dep.Dependency{
		{NWO: "", Ref: "v1", SHA: "abc"},
	}
	rewrites, _, err := r.ReverseLookup(context.Background(), deps)
	if err != nil {
		t.Fatal(err)
	}
	if len(rewrites) != 0 {
		t.Fatalf("expected no rewrites for empty NWO, got %v", rewrites)
	}
}

func TestReverseLookup_NoRewriteWhenRefUnchanged(t *testing.T) {
	// Test findContainingBranch directly — branch v1 has exact HEAD
	// match, so ref stays v1 and there's no rewrite.
	reg := &httpmock.Registry{}
	r, err := New("test.com", pinpool.New(1, nil), WithTransport(reg))
	if err != nil {
		t.Fatal(err)
	}
	candidates := []ghapi.BranchHead{
		{Name: "v1", SHA: "abc123"},
		{Name: "main", SHA: "other"},
	}
	got, err := r.findContainingBranch(context.Background(), "actions", "checkout", "abc123", "v1", "main", candidates)
	if err != nil {
		t.Fatal(err)
	}
	if got != "v1" {
		t.Fatalf("expected v1, got %q", got)
	}
}

// TestReverseLookup_KeepsSHAUseForTagWithAt: a tag such as predicate@2.0.0
// can be recorded as an entry's ref, but `uses:` can't name it.
func TestReverseLookup_KeepsSHAUseForTagWithAt(t *testing.T) {
	sha := strings.Repeat("a", 40)
	reg := &httpmock.Registry{}
	httpmock.NoTagsNamed(reg, "actions", "attest")
	reg.Register(httpmock.REST("GET", `repos/actions/attest/tags`), httpmock.JSONResponse(httpmock.TagListResponse("predicate@2.0.0", sha)))
	reg.Register(httpmock.REST("GET", `repos/actions/attest/branches`), httpmock.JSONResponse(httpmock.BranchListResponse("main", sha)))
	r, err := New("github.com", pinpool.New(2, nil), WithTransport(reg))
	require.NoError(t, err)
	deps := []dep.Dependency{{NWO: "actions/attest", Ref: sha, SHA: sha, HashAlgo: "sha1"}}

	rewrites, issues, err := r.ReverseLookup(context.Background(), deps)

	require.NoError(t, err)
	assert.Empty(t, issues)
	assert.Empty(t, rewrites)
	assert.Equal(t, sha, deps[0].Ref)
	assert.Equal(t, "predicate@2.0.0", deps[0].Tag)
}
