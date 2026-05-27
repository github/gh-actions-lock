package resolver

import (
	"strings"
	"testing"

	"github.com/github/gh-actions-pin/internal/httpmock"
	"github.com/github/gh-actions-pin/internal/lockfile"
)

func TestDiscoverContaining_PrefersHintTag(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", "actions/checkout/branch_commits/abc"),
		httpmock.JSONResponse(map[string]any{
			"branches": []map[string]any{{"branch": "main"}, {"branch": "releases/v4"}},
			"tags":     []string{"v4", "v4.2.1", "v4.2.2"},
		}),
	)

	r, err := NewWithTransport("github.com", reg)
	if err != nil {
		t.Fatal(err)
	}

	tag, branch, err := r.DiscoverContaining("actions", "checkout", "abc", "v4.2.2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tag != "v4.2.2" {
		t.Fatalf("expected tag=v4.2.2 (hint match), got %q", tag)
	}
	if branch != "main" {
		t.Fatalf("expected branch=main (lex-first), got %q", branch)
	}
	reg.Verify(t)
}

func TestDiscoverContaining_BranchOnly(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", "actions/checkout/branch_commits/def"),
		httpmock.JSONResponse(map[string]any{
			"branches": []map[string]any{{"branch": "main"}},
			"tags":     []string{},
		}),
	)

	r, err := NewWithTransport("github.com", reg)
	if err != nil {
		t.Fatal(err)
	}

	tag, branch, err := r.DiscoverContaining("actions", "checkout", "def", "main")
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
	reg.Register(
		httpmock.REST("GET", "actions/checkout/branch_commits/dead"),
		httpmock.JSONResponse(map[string]any{
			"branches": []any{},
			"tags":     []string{"poisoned"},
		}),
	)

	r, err := NewWithTransport("github.com", reg)
	if err != nil {
		t.Fatal(err)
	}

	_, _, err = r.DiscoverContaining("actions", "checkout", "dead", "poisoned")
	if err == nil {
		t.Fatalf("expected error for commit with no branches")
	}
	if !strings.Contains(err.Error(), "impostor") {
		t.Fatalf("expected impostor-signal error, got %v", err)
	}
	reg.Verify(t)
}

func TestDiscoverContainingDefault_PrefersDefaultBranch(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", "actions/checkout/branch_commits/abc"),
		httpmock.JSONResponse(map[string]any{
			"branches": []map[string]any{{"branch": "feature/x"}, {"branch": "main"}, {"branch": "zzz"}},
			"tags":     []string{},
		}),
	)

	r, err := NewWithTransport("github.com", reg)
	if err != nil {
		t.Fatal(err)
	}

	_, branch, err := r.DiscoverContainingDefault("actions", "checkout", "abc", "", "main")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if branch != "main" {
		t.Fatalf("expected branch=main (default), got %q", branch)
	}
	reg.Verify(t)
}

func TestNormalizeContaining_PopulatesTagBranchAndRewritesSHAPins(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", "actions/checkout/branch_commits/abc123"),
		httpmock.JSONResponse(map[string]any{
			"branches": []map[string]any{{"branch": "main"}},
			"tags":     []string{"v4"},
		}),
	)

	r, err := NewWithTransport("github.com", reg)
	if err != nil {
		t.Fatal(err)
	}

	deps := []lockfile.Dependency{
		{NWO: "actions/checkout", Ref: "abc123abc123abc123abc123abc123abc123abc1", SHA: "abc123", HashAlgo: "sha1"},
	}

	rewrites, err := r.NormalizeContaining(deps)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := deps[0].Tag; got != "v4" {
		t.Errorf("expected Tag=v4, got %q", got)
	}
	if got := deps[0].Branch; got != "main" {
		t.Errorf("expected Branch=main, got %q", got)
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

func TestNormalizeContaining_NoChangeWhenRefAlreadyCanonical(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", "actions/checkout/branch_commits/abc"),
		httpmock.JSONResponse(map[string]any{
			"branches": []map[string]any{{"branch": "main"}},
			"tags":     []string{"v4"},
		}),
	)

	r, err := NewWithTransport("github.com", reg)
	if err != nil {
		t.Fatal(err)
	}

	deps := []lockfile.Dependency{
		{NWO: "actions/checkout", Ref: "v4", SHA: "abc", HashAlgo: "sha1"},
	}

	rewrites, err := r.NormalizeContaining(deps)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rewrites) != 0 {
		t.Errorf("expected no rewrites when ref already canonical, got %v", rewrites)
	}
	if deps[0].Ref != "v4" {
		t.Errorf("ref should be unchanged, got %q", deps[0].Ref)
	}
	if deps[0].Tag != "v4" || deps[0].Branch != "main" {
		t.Errorf("expected Tag=v4 Branch=main, got Tag=%q Branch=%q", deps[0].Tag, deps[0].Branch)
	}
	reg.Verify(t)
}

func TestNormalizeContaining_SkippedWhenReachabilityDisabled(t *testing.T) {
	r, err := NewWithTransport("github.com", &httpmock.Registry{})
	if err != nil {
		t.Fatal(err)
	}
	r.DisableReachability = true

	deps := []lockfile.Dependency{
		{NWO: "actions/checkout", Ref: "v4", SHA: "abc"},
	}
	rewrites, err := r.NormalizeContaining(deps)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rewrites) != 0 {
		t.Errorf("expected no rewrites when disabled, got %v", rewrites)
	}
	if deps[0].Tag != "" || deps[0].Branch != "" {
		t.Errorf("expected Tag/Branch unset when disabled, got Tag=%q Branch=%q", deps[0].Tag, deps[0].Branch)
	}
}

func TestNormalizeContaining_FailsClosedOnImpostor(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", "actions/checkout/branch_commits/dead"),
		httpmock.JSONResponse(map[string]any{
			"branches": []any{},
			"tags":     []string{"poisoned"},
		}),
	)
	r, err := NewWithTransport("github.com", reg)
	if err != nil {
		t.Fatal(err)
	}

	deps := []lockfile.Dependency{
		{NWO: "actions/checkout", Ref: "poisoned", SHA: "dead"},
	}
	_, err = r.NormalizeContaining(deps)
	if err == nil {
		t.Fatalf("expected fail-closed error, got nil")
	}
	reg.Verify(t)
}
