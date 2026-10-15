package resolve

import (
	"context"
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
			defer reg.Verify(t)
			reg.Register(httpmock.REST("GET", `repos/owner/action$`),
				httpmock.JSONResponse(map[string]any{"default_branch": "main"}))
			reg.Register(httpmock.REST("GET", `repos/owner/action/git/ref/heads/main`),
				httpmock.JSONResponse(map[string]any{
					"ref": "refs/heads/main", "object": map[string]any{"sha": headSHA, "type": "commit"},
				}))
			if tt.sha != headSHA {
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
			assert.Equal(t, "main", deps[0].Branch)
			assert.Equal(t, tt.tag, deps[0].Tag)
			if tt.wantRef == tt.sha {
				assert.Empty(t, rewrites)
			} else {
				assert.Equal(t, map[string]string{"owner/action@" + tt.sha: "owner/action@" + tt.wantRef}, rewrites)
			}
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
