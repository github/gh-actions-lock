package resolver

import (
	"strings"
	"testing"

	"github.com/github/gh-actions-pin/internal/httpmock"
	"github.com/github/gh-actions-pin/internal/lockfile"
)

// branchListResponse returns a REST branches response for httpmock. All
// entries are marked protected:true so tests model the trusted-upstream
// case by default. Use branchListResponseProtected to mix protected and
// unprotected branches.
func branchListResponse(pairs ...string) any {
	out := make([]map[string]any, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, map[string]any{
			"name":      pairs[i],
			"commit":    map[string]any{"sha": pairs[i+1]},
			"protected": true,
		})
	}
	return out
}

// branchListResponseProtected returns a REST branches response where each
// entry is a (name, sha, protected) triple.
func branchListResponseProtected(triples ...any) any {
	out := make([]map[string]any, 0, len(triples)/3)
	for i := 0; i+2 < len(triples); i += 3 {
		out = append(out, map[string]any{
			"name":      triples[i],
			"commit":    map[string]any{"sha": triples[i+1]},
			"protected": triples[i+2],
		})
	}
	return out
}

// tagListResponse returns a REST tags response for httpmock.
func tagListResponse(pairs ...string) any {
	out := make([]map[string]any, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, map[string]any{
			"name":   pairs[i],
			"commit": map[string]any{"sha": pairs[i+1]},
		})
	}
	return out
}

// compareResponse returns a Compare API response indicating sha is an ancestor of the head.
func compareAncestorResponse(mergeBaseSHA string) any {
	return map[string]any{
		"status":            "ahead",
		"merge_base_commit": map[string]any{"sha": mergeBaseSHA},
	}
}

func TestDiscoverContaining_PrefersHintTag(t *testing.T) {
	// sha="abc", hintRef="v4.2.2" (a tag, not a branch name)
	// Neither branch HEAD matches "abc", so compare is needed.
	// orderedBranches with hintRef="v4.2.2" (not a branch) and no default:
	// lex order → main first → compare(abc...m000) matches.
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/branches`),
		httpmock.JSONResponse(branchListResponse("main", "m000", "releases/v4", "rv4000")),
	)
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/compare/abc`),
		httpmock.JSONResponse(compareAncestorResponse("abc")),
	)
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/tags`),
		httpmock.JSONResponse(tagListResponse("v4", "abc", "v4.2.1", "abc", "v4.2.2", "abc")),
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
		t.Fatalf("expected branch=main (lex-first via compare), got %q", branch)
	}
	reg.Verify(t)
}

func TestDiscoverContaining_BranchOnly(t *testing.T) {
	// sha="def", hintRef="main". main HEAD == "def" → exact match, no compare.
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/branches`),
		httpmock.JSONResponse(branchListResponse("main", "def")),
	)
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/tags`),
		httpmock.JSONResponse(tagListResponse()),
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
		httpmock.REST("GET", `repos/actions/checkout/branches`),
		httpmock.JSONResponse(branchListResponse()),
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
	// sha="abc", hintRef="", defaultBranch="main".
	// No exact match → compare path. orderedBranches puts main first.
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/branches`),
		httpmock.JSONResponse(branchListResponse("feature/x", "fx000", "main", "m000", "zzz", "z000")),
	)
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/compare/abc`),
		httpmock.JSONResponse(compareAncestorResponse("abc")),
	)
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/tags`),
		httpmock.JSONResponse(tagListResponse()),
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

func TestDiscoverContaining_AutoDiscoversDefaultAndPrefersProtected(t *testing.T) {
	// No hintRef, no exact-match branch. Lex order would try aaa first,
	// but we expect: main (default, fetched from repo metadata) →
	// releases/v4 (protected) → aaa (unprotected). main and releases/v4
	// don't contain "abc"; releases/v4 does. So compare order matters:
	// main first (404 ancestry), then releases/v4 (match).
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/branches`),
		httpmock.JSONResponse(branchListResponseProtected(
			"aaa", "aaa000", false,
			"main", "m000", true,
			"releases/v4", "rv4000", true,
			"zzz", "z000", false,
		)),
	)
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout$`),
		httpmock.JSONResponse(map[string]any{"default_branch": "main"}),
	)
	// First compare hit: abc...m000 (default branch) — not an ancestor.
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/compare/abc\.\.\.m000`),
		httpmock.JSONResponse(compareAncestorResponse("0000")),
	)
	// Second compare hit: abc...rv4000 (protected) — match.
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/compare/abc\.\.\.rv4000`),
		httpmock.JSONResponse(compareAncestorResponse("abc")),
	)
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/tags`),
		httpmock.JSONResponse(tagListResponse()),
	)

	r, err := NewWithTransport("github.com", reg)
	if err != nil {
		t.Fatal(err)
	}

	_, branch, err := r.DiscoverContaining("actions", "checkout", "abc", "")
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
		httpmock.JSONResponse(branchListResponseProtected(
			"main", "abc", false,
			"dev", "d000", false,
		)),
	)
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/tags`),
		httpmock.JSONResponse(tagListResponse()),
	)

	r, err := NewWithTransport("github.com", reg)
	if err != nil {
		t.Fatal(err)
	}

	_, branch, err := r.DiscoverContaining("actions", "checkout", "abc", "main")
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
	// contains abc. Protected-first fails → fallback finds dev.
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/branches`),
		httpmock.JSONResponse(branchListResponseProtected(
			"main", "m000", true,
			"dev", "abc", false,
		)),
	)
	// First pass scans main (protected) via Compare — not an ancestor.
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/compare/abc\.\.\.m000`),
		httpmock.JSONResponse(compareAncestorResponse("0000")),
	)
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/tags`),
		httpmock.JSONResponse(tagListResponse()),
	)

	r, err := NewWithTransport("github.com", reg)
	if err != nil {
		t.Fatal(err)
	}

	_, branch, err := r.DiscoverContaining("actions", "checkout", "abc", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if branch != "dev" {
		t.Fatalf("expected branch=dev (fallback exact-match), got %q", branch)
	}
	reg.Verify(t)
}

func TestNormalizeContaining_PopulatesTagBranchAndRewritesSHAPins(t *testing.T) {
	// SHA pin: dep.SHA="abc123", dep.Ref=full-sha. main HEAD=="abc123" → exact match.
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/branches`),
		httpmock.JSONResponse(branchListResponse("main", "abc123")),
	)
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/tags`),
		httpmock.JSONResponse(tagListResponse("v4", "abc123")),
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
	// dep.Ref="v4", dep.SHA="abc". main HEAD=="abc" → exact match.
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/branches`),
		httpmock.JSONResponse(branchListResponse("main", "abc")),
	)
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/tags`),
		httpmock.JSONResponse(tagListResponse("v4", "abc")),
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

func TestNormalizeContaining_DisableReachabilityDoesNotAffectBranchDiscovery(t *testing.T) {
	// DisableReachability only gates CheckReachability, not NormalizeContaining.
	// Branch discovery for lockfile metadata always runs.
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/branches`),
		httpmock.JSONResponse(branchListResponse("main", "abc")),
	)
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/tags`),
		httpmock.JSONResponse(tagListResponse("v4", "abc")),
	)

	r, err := NewWithTransport("github.com", reg)
	if err != nil {
		t.Fatal(err)
	}
	r.DisableReachability = true

	deps := []lockfile.Dependency{
		{NWO: "actions/checkout", Ref: "v4", SHA: "abc"},
	}
	_, err = r.NormalizeContaining(deps)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if deps[0].Branch != "main" {
		t.Errorf("expected Branch=main even when DisableReachability=true, got %q", deps[0].Branch)
	}
	reg.Verify(t)
}

func TestNormalizeContaining_FailsClosedOnImpostor(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/branches`),
		httpmock.JSONResponse(branchListResponse()),
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

func TestNormalizeContaining_PreservesBranchRefOverTag(t *testing.T) {
	// User wrote @main — main HEAD=="abc" → exact match.
	// Tag discovery finds v4 and v4.3.1 but branch ref is preserved.
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/branches`),
		httpmock.JSONResponse(branchListResponse("main", "abc", "releases/v4", "xyz")),
	)
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/tags`),
		httpmock.JSONResponse(tagListResponse("v4", "abc", "v4.3.1", "abc")),
	)

	r, err := NewWithTransport("github.com", reg)
	if err != nil {
		t.Fatal(err)
	}

	deps := []lockfile.Dependency{
		{NWO: "actions/checkout", Ref: "main", SHA: "abc", HashAlgo: "sha1"},
	}

	rewrites, err := r.NormalizeContaining(deps)
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
