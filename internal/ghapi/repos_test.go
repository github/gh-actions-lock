package ghapi

import (
	"context"
	"net/http"
	"testing"

	"github.com/github/gh-actions-pin/internal/ghapi/httpmock"
)

func TestOrderedBranches(t *testing.T) {
	branches := []BranchHead{
		{Name: "dev", SHA: "1", Protected: false},
		{Name: "main", SHA: "2", Protected: true},
		{Name: "release/v4", SHA: "3", Protected: true},
		{Name: "alpha", SHA: "4", Protected: false},
		{Name: "staging", SHA: "5", Protected: true},
	}

	ordered := OrderedBranches(branches, "release/v4", "v4", "main")

	want := []string{"release/v4", "main", "staging", "alpha", "dev"}
	if len(ordered) != len(want) {
		t.Fatalf("expected %d branches, got %d", len(want), len(ordered))
	}
	for i, name := range want {
		if ordered[i].Name != name {
			t.Errorf("position %d: expected %q, got %q", i, name, ordered[i].Name)
		}
	}
}

func TestOrderedBranches_EmptyHints(t *testing.T) {
	branches := []BranchHead{
		{Name: "b", SHA: "1"},
		{Name: "a", SHA: "2"},
	}
	ordered := OrderedBranches(branches, "", "", "")
	if len(ordered) != 2 {
		t.Fatalf("expected 2, got %d", len(ordered))
	}
	if ordered[0].Name != "a" {
		t.Errorf("expected lex-sorted, got %q first", ordered[0].Name)
	}
}

func TestEscapeBranchPath(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"main", "main"},
		{"release/v4", "release/v4"},
		{"feat/hello world", "feat/hello%20world"},
	}
	for _, tt := range tests {
		got := escapeBranchPath(tt.in)
		if got != tt.want {
			t.Errorf("escapeBranchPath(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestCompareCommits_SameSHA(t *testing.T) {
	c := &Client{}
	ok, err := c.CompareCommits(context.Background(), "o", "r", "abc", "abc")
	if err != nil || !ok {
		t.Fatalf("expected true/nil for same SHA, got %v/%v", ok, err)
	}
}

func TestCompareCommits_Ancestor(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", "repos/o/r/compare/old...new"),
		httpmock.JSONResponse(map[string]any{
			"status":            "behind",
			"merge_base_commit": map[string]string{"sha": "old"},
		}),
	)

	c := newTestClient(t, reg)
	ok, err := c.CompareCommits(context.Background(), "o", "r", "old", "new")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected true for ancestor")
	}
}

func TestCompareCommits_NotAncestor(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", "repos/o/r/compare/old...new"),
		httpmock.JSONResponse(map[string]any{
			"status":            "diverged",
			"merge_base_commit": map[string]string{"sha": "other"},
		}),
	)

	c := newTestClient(t, reg)
	ok, err := c.CompareCommits(context.Background(), "o", "r", "old", "new")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected false for non-ancestor")
	}
}

func TestCompareCommits_404(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", "repos/o/r/compare/old...new"),
		httpmock.StatusResponse(http.StatusNotFound),
	)

	c := newTestClient(t, reg)
	ok, err := c.CompareCommits(context.Background(), "o", "r", "old", "new")
	if err != nil {
		t.Fatalf("expected nil error for 404, got %v", err)
	}
	if ok {
		t.Fatal("expected false for 404")
	}
}

func TestCompareCommits_Cache(t *testing.T) {
	calls := 0
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", "repos/o/r/compare/old...new"),
		func(req *http.Request) (*http.Response, error) {
			calls++
			return httpmock.JSONResponse(map[string]any{
				"status":            "behind",
				"merge_base_commit": map[string]string{"sha": "old"},
			})(req)
		},
	)

	c := newTestClient(t, reg)
	_, _ = c.CompareCommits(context.Background(), "o", "r", "old", "new")
	_, _ = c.CompareCommits(context.Background(), "o", "r", "old", "new")
	if calls != 1 {
		t.Fatalf("expected 1 call (second should cache), got %d", calls)
	}
}

func TestGetDefaultBranch(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", "repos/o/r"),
		httpmock.JSONResponse(map[string]any{"default_branch": "main"}),
	)

	c := newTestClient(t, reg)
	got := c.GetDefaultBranch(context.Background(), "o", "r")
	if got != "main" {
		t.Fatalf("expected main, got %q", got)
	}
}

func TestListBranches(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", "repos/o/r/branches"),
		httpmock.JSONResponse([]map[string]any{
			{"name": "main", "commit": map[string]string{"sha": "aaa"}, "protected": true},
			{"name": "dev", "commit": map[string]string{"sha": "bbb"}, "protected": false},
		}),
	)

	c := newTestClient(t, reg)
	branches, err := c.ListBranches(context.Background(), "o", "r")
	if err != nil {
		t.Fatal(err)
	}
	if len(branches) != 2 {
		t.Fatalf("expected 2 branches, got %d", len(branches))
	}
	if branches[0].Name != "main" || !branches[0].Protected {
		t.Fatalf("unexpected first branch: %+v", branches[0])
	}
}

func TestRepoIDs(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", "repos/o/r"),
		httpmock.JSONResponse(map[string]any{
			"id":    42,
			"owner": map[string]any{"id": 7},
		}),
	)

	c := newTestClient(t, reg)
	ownerID, repoID, err := c.RepoIDs(context.Background(), "o", "r")
	if err != nil {
		t.Fatal(err)
	}
	if ownerID != 7 || repoID != 42 {
		t.Fatalf("expected owner=7 repo=42, got owner=%d repo=%d", ownerID, repoID)
	}
}

func TestRepoIDs_ZeroIDs(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", "repos/o/r"),
		httpmock.JSONResponse(map[string]any{
			"id":    0,
			"owner": map[string]any{"id": 0},
		}),
	)

	c := newTestClient(t, reg)
	_, _, err := c.RepoIDs(context.Background(), "o", "r")
	if err == nil {
		t.Fatal("expected error for zero IDs")
	}
}

func TestListTags(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", "repos/o/r/tags"),
		httpmock.JSONResponse([]map[string]any{
			{"name": "v1.0.0", "commit": map[string]string{"sha": "aaa"}},
			{"name": "v2.0.0", "commit": map[string]string{"sha": "bbb"}},
		}),
	)

	c := newTestClient(t, reg)
	tags, err := c.ListTags(context.Background(), "o", "r")
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) != 2 {
		t.Fatalf("expected 2 tags, got %d", len(tags))
	}
	if tags[0].Name != "v1.0.0" {
		t.Fatalf("unexpected first tag: %+v", tags[0])
	}
}

func TestGetBranchHead(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", "repos/o/r/git/ref/heads/main"),
		httpmock.JSONResponse(map[string]any{
			"ref":    "refs/heads/main",
			"object": map[string]any{"sha": "abc123", "type": "commit"},
		}),
	)

	c := newTestClient(t, reg)
	bh, ok := c.GetBranchHead(context.Background(), "o", "r", "main")
	if !ok {
		t.Fatal("expected ok=true")
	}
	if bh.Name != "main" || bh.SHA != "abc123" {
		t.Fatalf("unexpected result: %+v", bh)
	}
}

func TestGetBranchHead_Empty(t *testing.T) {
	c := &Client{}
	_, ok := c.GetBranchHead(context.Background(), "o", "r", "")
	if ok {
		t.Fatal("expected ok=false for empty name")
	}
}

func TestMatchingHeadRefs(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", "repos/o/r/git/matching-refs/heads/release/"),
		httpmock.JSONResponse([]map[string]any{
			{"ref": "refs/heads/release/v3", "object": map[string]any{"sha": "aaa", "type": "commit"}},
			{"ref": "refs/heads/release/v4", "object": map[string]any{"sha": "bbb", "type": "commit"}},
		}),
	)

	c := newTestClient(t, reg)
	branches := c.MatchingHeadRefs(context.Background(), "o", "r", "release/")
	if len(branches) != 2 {
		t.Fatalf("expected 2 branches, got %d", len(branches))
	}
	if branches[0].Name != "release/v3" {
		t.Fatalf("unexpected first branch: %+v", branches[0])
	}
}
