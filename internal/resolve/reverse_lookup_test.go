package resolve

import (
	"context"
	"testing"

	"github.com/github/gh-actions-lock/internal/dep"
	"github.com/github/gh-actions-lock/internal/ghapi"
	"github.com/github/gh-actions-lock/internal/ghapi/httpmock"
	"github.com/github/gh-actions-lock/internal/pinpool"
)

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
	rewrites, err := r.ReverseLookup(context.Background(), deps)
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
