package resolve

import (
	"context"
	"strings"
	"testing"

	"github.com/github/gh-actions-pin/internal/dep"
	"github.com/github/gh-actions-pin/internal/ghapi/httpmock"
	"github.com/github/gh-actions-pin/internal/pinpool"
)

func TestDiscoverContaining_PrefersHintTag(t *testing.T) {
	// sha="abc", hintRef="v4.2.2" (a tag, not a branch name)
	// Neither branch HEAD matches "abc", so compare is needed.
	// orderedBranches with hintRef="v4.2.2" (not a branch) and no default:
	// lex order → main first → compare(abc...m000) matches.
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/branches`),
		httpmock.JSONResponse(httpmock.BranchListResponse("main", "m000", "releases/v4", "rv4000")),
	)
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/compare/abc`),
		httpmock.JSONResponse(httpmock.CompareAncestorResponse("abc")),
	)
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
	if branch != "main" {
		t.Fatalf("expected branch=main (lex-first via compare), got %q", branch)
	}
	reg.Verify(t)
}

func TestDiscoverContaining_BranchOnly(t *testing.T) {
	// sha="def", hintRef="main". main HEAD == "def" → exact match, no compare.
	reg := &httpmock.Registry{}
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
	// Phase 2: listBranches (full listing) also returns empty → impostor error.
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/branches`),
		httpmock.JSONResponse(httpmock.BranchListResponse()),
	)

	r, err := New("github.com", pinpool.New(2, nil), WithTransport(reg))
	if err != nil {
		t.Fatal(err)
	}

	_, _, err = r.DiscoverContaining(context.Background(), "actions", "checkout", "dead", "poisoned")
	if err == nil {
		t.Fatalf("expected error for commit with no branches")
	}
	if !strings.Contains(err.Error(), "impostor") {
		t.Fatalf("expected impostor-signal error, got %v", err)
	}
	reg.Verify(t)
}

func TestDiscoverContainingDefault_PrefersDefaultBranch(t *testing.T) {
	// sha="abc", hintRef="", defaultBranch="main".
	// No exact match → compare path. orderedBranches puts main first.
	reg := &httpmock.Registry{}
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

func TestReverseLookup_PopulatesTagBranchAndRewritesSHAPins(t *testing.T) {
	// SHA pin: dep.SHA="abc123", dep.Ref=full-sha. main HEAD=="abc123" → exact match.
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/branches`),
		httpmock.JSONResponse(httpmock.BranchListResponse("main", "abc123")),
	)
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

	rewrites, err := r.ReverseLookup(context.Background(), deps)
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

func TestReverseLookup_NoChangeWhenRefAlreadyCanonical(t *testing.T) {
	// dep.Ref="v4", dep.SHA="abc". main HEAD=="abc" → exact match.
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/branches`),
		httpmock.JSONResponse(httpmock.BranchListResponse("main", "abc")),
	)
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/tags`),
		httpmock.JSONResponse(httpmock.TagListResponse("v4", "abc")),
	)

	r, err := New("github.com", pinpool.New(2, nil), WithTransport(reg))
	if err != nil {
		t.Fatal(err)
	}

	deps := []dep.Dependency{
		{NWO: "actions/checkout", Ref: "v4", SHA: "abc", HashAlgo: "sha1"},
	}

	rewrites, err := r.ReverseLookup(context.Background(), deps)
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

func TestReverseLookup_FailsClosedOnImpostor(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/branches`),
		httpmock.JSONResponse(httpmock.BranchListResponse()),
	)
	r, err := New("github.com", pinpool.New(2, nil), WithTransport(reg))
	if err != nil {
		t.Fatal(err)
	}

	deps := []dep.Dependency{
		{NWO: "actions/checkout", Ref: "poisoned", SHA: "dead"},
	}
	_, err = r.ReverseLookup(context.Background(), deps)
	if err == nil {
		t.Fatalf("expected fail-closed error, got nil")
	}
	reg.Verify(t)
}

func TestReverseLookup_PreservesBranchRefOverTag(t *testing.T) {
	// User wrote @main — main HEAD=="abc" → exact match.
	// Tag discovery finds v4 and v4.3.1 but branch ref is preserved.
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/branches`),
		httpmock.JSONResponse(httpmock.BranchListResponse("main", "abc", "releases/v4", "xyz")),
	)
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/tags`),
		httpmock.JSONResponse(httpmock.TagListResponse("v4", "abc", "v4.3.1", "abc")),
	)

	r, err := New("github.com", pinpool.New(2, nil), WithTransport(reg))
	if err != nil {
		t.Fatal(err)
	}

	deps := []dep.Dependency{
		{NWO: "actions/checkout", Ref: "main", SHA: "abc", HashAlgo: "sha1"},
	}

	rewrites, err := r.ReverseLookup(context.Background(), deps)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rewrites) != 0 {
		t.Errorf("expected no rewrites when user wrote a branch ref, got %v", rewrites)
	}
	if deps[0].Ref != "main" {
		t.Errorf("expected Ref=main (preserved), got %q", deps[0].Ref)
	}
	if deps[0].Tag != "v4.3.1" {
		t.Errorf("expected Tag populated (highest semver), got %q", deps[0].Tag)
	}
	if deps[0].Branch != "main" {
		t.Errorf("expected Branch=main, got %q", deps[0].Branch)
	}
	reg.Verify(t)
}
