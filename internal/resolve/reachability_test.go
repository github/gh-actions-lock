package resolve

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/github/gh-actions-pin/internal/ghapi"
	"github.com/github/gh-actions-pin/internal/ghapi/httpmock"
)

func TestCheckReachability_CacheHit(t *testing.T) {
	r := &Resolver{}
	r.reachCache.Put(
		ghapi.ForReach("actions", "checkout", "abc123", "v4"),
		reachCacheEntry{status: Reachable, detail: "cached hit"},
	)

	result := r.CheckReachability(context.Background(), "actions", "checkout", "abc123", "v4")
	if result.Status != Reachable {
		t.Fatalf("expected Reachable, got %v", result.Status)
	}
	if result.Detail != "cached hit" {
		t.Fatalf("unexpected detail: %s", result.Detail)
	}
}

func TestCheckReachability_InjectedFn(t *testing.T) {
	r := &Resolver{}
	r.checkReachFn = func(_ context.Context, owner, repo, sha, ref string) (ReachabilityStatus, string) {
		return Unreachable, "injected: not on any branch"
	}

	result := r.CheckReachability(context.Background(), "actions", "checkout", "abc123", "v4")
	if result.Status != Unreachable {
		t.Fatalf("expected Unreachable, got %v", result.Status)
	}
	if result.Detail != "injected: not on any branch" {
		t.Fatalf("unexpected detail: %s", result.Detail)
	}

	// Should be cached after injected fn runs.
	status, detail, ok := r.getReachCache("actions", "checkout", "abc123", "v4")
	if !ok {
		t.Fatal("expected cache entry after injected fn")
	}
	if status != Unreachable || detail != "injected: not on any branch" {
		t.Fatalf("unexpected cached values: %v %q", status, detail)
	}
}

func TestCheckReachability_Singleflight(t *testing.T) {
	calls := 0
	r := &Resolver{}
	r.checkReachFn = func(_ context.Context, _, _, _, _ string) (ReachabilityStatus, string) {
		calls++
		return Reachable, "ok"
	}

	// Two serial calls with same key — second should hit cache, not fn.
	_ = r.CheckReachability(context.Background(), "o", "r", "sha", "ref")
	_ = r.CheckReachability(context.Background(), "o", "r", "sha", "ref")

	if calls != 1 {
		t.Fatalf("expected 1 fn call (second should cache hit), got %d", calls)
	}
}

// --- reachabilityScan primitive tests -------------------------------------
//
// These drive the security-critical scan directly through httpmock (no
// checkReachFn injection), so they exercise the real GraphQL/REST decision
// logic. The scan must distinguish three outcomes that the classifier
// depends on: matched (reachable), checked-but-unmatched (unreachable), and
// not-checked (unknown / fail-open).

const (
	scanSHA  = "1111111111111111111111111111111111111111"
	scanHead = "2222222222222222222222222222222222222222"
)

// branchCompareResponse builds a GraphQL batch-compare response body. Each
// status maps positionally to alias b0, b1, ... matching the query builder.
func branchCompareResponse(statuses ...string) map[string]any {
	repo := map[string]any{}
	for i, st := range statuses {
		repo[fmt.Sprintf("b%d", i)] = map[string]any{
			"compare": map[string]any{"status": st},
		}
	}
	return map[string]any{"data": map[string]any{"repo": repo}}
}

// registerEmptyCanonical stubs the phase-1/phase-2 branch probes for an
// arbitrary owner/repo, all empty, forcing the scan to rely on whatever the
// caller stubbed for phase 0.
func registerEmptyCanonical(reg *httpmock.Registry, owner, repo string) {
	base := fmt.Sprintf("repos/%s/%s", owner, repo)
	reg.Register(
		httpmock.RESTWithQuery("GET", base+"/branches", "protected=true"),
		httpmock.JSONResponse([]any{}),
	)
	reg.Register(
		httpmock.REST("GET", base+"/git/matching-refs/heads/v"),
		httpmock.JSONResponse([]any{}),
	)
	reg.Register(
		httpmock.REST("GET", base+"/git/matching-refs/heads/release"),
		httpmock.JSONResponse([]any{}),
	)
	reg.Register(
		httpmock.REST("GET", base+"/branches"),
		httpmock.JSONResponse([]any{}),
	)
}

func TestReachabilityScan_ExactHeadMatch(t *testing.T) {
	// HEAD SHA equals the pinned SHA — must match with zero API calls.
	r := &Resolver{}
	cands := []ghapi.BranchHead{{Name: "main", SHA: scanSHA}}

	matched, checked := r.reachabilityScan(context.Background(), "acme", "widget", scanSHA, scanSHA, cands, "main")
	if matched != "main" || !checked {
		t.Fatalf("exact-head: got (%q, %v), want (main, true)", matched, checked)
	}
}

func TestReachabilityScan_AncestorViaGraphQL(t *testing.T) {
	// HEAD differs; GraphQL compare reports BEHIND => sha is an ancestor.
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.GraphQLForRepo("acme", "widget"),
		httpmock.JSONResponse(branchCompareResponse("BEHIND")),
	)
	r := newTestResolver(t, reg)
	cands := []ghapi.BranchHead{{Name: "main", SHA: scanHead}}

	matched, checked := r.reachabilityScan(context.Background(), "acme", "widget", scanSHA, scanSHA, cands, "main")
	if matched != "main" || !checked {
		t.Fatalf("ancestor: got (%q, %v), want (main, true)", matched, checked)
	}
	reg.Verify(t)
}

func TestReachabilityScan_CheckedButNotFound(t *testing.T) {
	// GraphQL succeeds but reports DIVERGED => checked, not reachable.
	// This is the "faithful impostor" signal the classifier turns into
	// Unreachable.
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.GraphQLForRepo("acme", "widget"),
		httpmock.JSONResponse(branchCompareResponse("DIVERGED")),
	)
	r := newTestResolver(t, reg)
	cands := []ghapi.BranchHead{{Name: "main", SHA: scanHead}}

	matched, checked := r.reachabilityScan(context.Background(), "acme", "widget", scanSHA, scanSHA, cands, "main")
	if matched != "" || !checked {
		t.Fatalf("diverged: got (%q, %v), want (\"\", true)", matched, checked)
	}
	reg.Verify(t)
}

func TestReachabilityScan_TotalFailureUnknown(t *testing.T) {
	// GraphQL transport failure AND the REST compare fallback also fails =>
	// nothing was checked. The classifier must NOT treat this as
	// unreachable (fail-open to Unknown).
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.GraphQLForRepo("acme", "widget"),
		httpmock.StatusResponse(500),
	)
	reg.Register(
		httpmock.REST("GET", `repos/acme/widget/compare/`),
		httpmock.StatusResponse(500),
	)
	r := newTestResolver(t, reg)
	cands := []ghapi.BranchHead{{Name: "main", SHA: scanHead}}

	matched, checked := r.reachabilityScan(context.Background(), "acme", "widget", scanSHA, scanSHA, cands, "main")
	if matched != "" || checked {
		t.Fatalf("total-failure: got (%q, %v), want (\"\", false)", matched, checked)
	}
	reg.Verify(t)
}

func TestReachabilityScan_RESTFallbackMatch(t *testing.T) {
	// GraphQL fails entirely; the REST Compare fallback succeeds and the
	// merge-base equals sha => reachable via the legacy path.
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.GraphQLForRepo("acme", "widget"),
		httpmock.StatusResponse(500),
	)
	reg.Register(
		httpmock.REST("GET", `repos/acme/widget/compare/`),
		httpmock.JSONResponse(map[string]any{
			"status":            "behind",
			"merge_base_commit": map[string]any{"sha": scanSHA},
		}),
	)
	r := newTestResolver(t, reg)
	cands := []ghapi.BranchHead{{Name: "main", SHA: scanHead}}

	matched, checked := r.reachabilityScan(context.Background(), "acme", "widget", scanSHA, scanSHA, cands, "main")
	if matched != "main" || !checked {
		t.Fatalf("rest-fallback: got (%q, %v), want (main, true)", matched, checked)
	}
	reg.Verify(t)
}

// --- checkReachabilityOnce end-to-end classification ----------------------
//
// These exercise the full orchestration (phase 0/1/2) through httpmock and
// assert the final ReachabilityStatus, locking the three security-relevant
// verdicts: Reachable, Unreachable, and the fail-open ReachabilityUnknown.

func TestCheckReachabilityOnce_ReachableExactHead(t *testing.T) {
	// Default branch HEAD == pinned SHA: phase 0 resolves it immediately, no
	// branch listing required.
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", `repos/acme/widget$`),
		httpmock.JSONResponse(map[string]any{"default_branch": "main"}),
	)
	reg.Register(
		httpmock.REST("GET", `repos/acme/widget/git/ref/heads/main`),
		httpmock.JSONResponse(gitRefHeadResponse("main", scanSHA)),
	)
	r := newTestResolver(t, reg)

	got := r.checkReachabilityOnce(context.Background(), "acme", "widget", scanSHA, scanSHA)
	if got.Status != Reachable {
		t.Fatalf("status = %v, want Reachable (detail=%q)", got.Status, got.Detail)
	}
	if !strings.Contains(got.Detail, "on branch main") {
		t.Fatalf("detail = %q, want mention of branch main", got.Detail)
	}
	reg.Verify(t)
}

func TestCheckReachabilityOnce_UnreachableImpostor(t *testing.T) {
	// HEAD differs and every branch the scan checks reports DIVERGED: the
	// commit was checked against the canonical history and is on no branch.
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", `repos/acme/widget$`),
		httpmock.JSONResponse(map[string]any{"default_branch": "main"}),
	)
	reg.Register(
		httpmock.REST("GET", `repos/acme/widget/git/ref/heads/main`),
		httpmock.JSONResponse(gitRefHeadResponse("main", scanHead)),
	)
	reg.Register(
		httpmock.GraphQLForRepo("acme", "widget"),
		httpmock.JSONResponse(branchCompareResponse("DIVERGED")),
	)
	registerEmptyCanonical(reg, "acme", "widget")
	r := newTestResolver(t, reg)

	got := r.checkReachabilityOnce(context.Background(), "acme", "widget", scanSHA, scanSHA)
	if got.Status != Unreachable {
		t.Fatalf("status = %v, want Unreachable (detail=%q)", got.Status, got.Detail)
	}
	if !strings.Contains(got.Detail, "NOT on any branch") {
		t.Fatalf("detail = %q, want NOT-on-any-branch message", got.Detail)
	}
	reg.Verify(t)
}

func TestCheckReachabilityOnce_RateLimitedUnknown(t *testing.T) {
	// GraphQL and the REST Compare fallback both fail; no branch is ever
	// successfully checked. The classifier must fail open to
	// ReachabilityUnknown rather than declaring the commit an impostor.
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", `repos/acme/widget$`),
		httpmock.JSONResponse(map[string]any{"default_branch": "main"}),
	)
	reg.Register(
		httpmock.REST("GET", `repos/acme/widget/git/ref/heads/main`),
		httpmock.JSONResponse(gitRefHeadResponse("main", scanHead)),
	)
	reg.Register(
		httpmock.GraphQLForRepo("acme", "widget"),
		httpmock.StatusResponse(500),
	)
	reg.Register(
		httpmock.REST("GET", `repos/acme/widget/compare/`),
		httpmock.StatusResponse(500),
	)
	registerEmptyCanonical(reg, "acme", "widget")
	r := newTestResolver(t, reg)

	got := r.checkReachabilityOnce(context.Background(), "acme", "widget", scanSHA, scanSHA)
	if got.Status != ReachabilityUnknown {
		t.Fatalf("status = %v, want ReachabilityUnknown (detail=%q)", got.Status, got.Detail)
	}
	reg.Verify(t)
}
