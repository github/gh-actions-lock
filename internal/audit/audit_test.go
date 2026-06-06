package audit

import (
"context"
"strconv"
"strings"
"testing"
"time"

"github.com/github/gh-actions-pin/internal/lockfile"
"github.com/github/gh-actions-pin/internal/resolve"
"github.com/github/gh-actions-pin/internal/httpmock"
)

func TestCheckReachability_Reachable(t *testing.T) {
	r := func() *resolve.Resolver {
		r := &resolve.Resolver{}
		r.SetCheckReachabilityFunc(func(_ context.Context, owner, repo, sha, ref string) (resolve.ReachabilityStatus, string) {
			return resolve.Reachable, "ancestor of " + ref
		})
		return r
		}()
	result := New(r).CheckReachability(context.Background(), "actions", "checkout", "abc123", "v6")
	if result.Status != resolve.Reachable {
		t.Fatalf("expected resolve.Reachable, got %s (%s)", result.Status, result.Detail)
	}
}

func TestCheckReachability_Unreachable(t *testing.T) {
	r := func() *resolve.Resolver {
		r := &resolve.Resolver{}
		r.SetCheckReachabilityFunc(func(_ context.Context, owner, repo, sha, ref string) (resolve.ReachabilityStatus, string) {
			return resolve.Unreachable, "commit is not an ancestor of " + ref
		})
		return r
		}()
	result := New(r).CheckReachability(context.Background(), "evil", "repo", "deadbeef", "v1")
	if result.Status != resolve.Unreachable {
		t.Fatalf("expected resolve.Unreachable, got %s (%s)", result.Status, result.Detail)
	}
}

func TestCheckReachability_Unknown(t *testing.T) {
	r := func() *resolve.Resolver {
		r := &resolve.Resolver{}
		r.SetCheckReachabilityFunc(func(_ context.Context, owner, repo, sha, ref string) (resolve.ReachabilityStatus, string) {
			return resolve.ReachabilityUnknown, "clone failed"
		})
		return r
		}()
	result := New(r).CheckReachability(context.Background(), "actions", "checkout", "abc123", "v6")
	if result.Status != resolve.ReachabilityUnknown {
		t.Fatalf("expected Unknown, got %s (%s)", result.Status, result.Detail)
	}
}

func TestCheckReachability_CachesResults(t *testing.T) {
	calls := 0
	r := func() *resolve.Resolver {
		r := &resolve.Resolver{}
		r.SetCheckReachabilityFunc(func(_ context.Context, owner, repo, sha, ref string) (resolve.ReachabilityStatus, string) {
			calls++
			return resolve.Reachable, "ancestor of " + ref
		})
		return r
		}()

	r1 := New(r).CheckReachability(context.Background(), "actions", "checkout", "abc123", "v6")
	r2 := New(r).CheckReachability(context.Background(), "actions", "checkout", "abc123", "v6")

	if r1.Status != resolve.Reachable || r2.Status != resolve.Reachable {
		t.Fatalf("expected both calls to return resolve.Reachable, got %s and %s", r1.Status, r2.Status)
	}
	if r2.Detail != "ancestor of v6" {
		t.Fatalf("expected second call to return the original detail, got %q", r2.Detail)
	}
	if calls != 1 {
		t.Fatalf("expected checkReachFn called once, got %d", calls)
	}
}

func TestCheckReachabilityAll_DeduplicatesRequests(t *testing.T) {
	calls := 0
	r := func() *resolve.Resolver {
		r := &resolve.Resolver{}
		r.SetCheckReachabilityFunc(func(_ context.Context, owner, repo, sha, ref string) (resolve.ReachabilityStatus, string) {
			calls++
			return resolve.Reachable, "ancestor of " + ref
		})
		return r
		}()

	deps := []lockfile.Dependency{
		{NWO: "actions/checkout", Ref: "v6", SHA: "aaa"},
		{NWO: "actions/checkout", Ref: "v6", SHA: "aaa"}, // duplicate
		{NWO: "actions/setup-go", Ref: "v6", SHA: "bbb"},
	}

	results := New(r).CheckReachabilityAll(context.Background(), deps)
	if len(results) != 2 {
		t.Fatalf("expected 2 unique results, got %d: %+v", len(results), results)
	}
	if calls != 2 {
		t.Fatalf("expected 2 calls (deduped), got %d", calls)
	}
}

func TestCheckReachability_SHAAsRef_ChecksViaBranchCommits(t *testing.T) {
	sha := "abc123abc123abc123abc123abc123abc123abc1"

	t.Run("on a branch", func(t *testing.T) {
		reg := &httpmock.Registry{}
		// Bare-SHA ref: main HEAD == sha → exact match → resolve.Reachable.
		reg.Register(
			httpmock.REST("GET", `repos/actions/checkout/branches`),
			httpmock.JSONResponse(httpmock.BranchListResponse("main", sha)),
		)
		r, err := resolve.NewWithTransport("github.com", reg)
		if err != nil {
			t.Fatal(err)
		}
		result := New(r).CheckReachability(context.Background(), "actions", "checkout", sha, sha)
		if result.Status != resolve.Reachable {
			t.Fatalf("expected resolve.Reachable, got %s (%s)", result.Status, result.Detail)
		}
		if !strings.Contains(result.Detail, "bare SHA") {
			t.Fatalf("expected detail to mention bare SHA, got %q", result.Detail)
		}
		reg.Verify(t)
	})

	t.Run("not on any branch", func(t *testing.T) {
		reg := &httpmock.Registry{}
		probeBranchesEmpty(reg)
		reg.Register(
			httpmock.REST("GET", `repos/actions/checkout/branches`),
			httpmock.JSONResponse(httpmock.BranchListResponse()),
		)
		r, err := resolve.NewWithTransport("github.com", reg)
		if err != nil {
			t.Fatal(err)
		}
		result := New(r).CheckReachability(context.Background(), "actions", "checkout", sha, sha)
		if result.Status != resolve.Unreachable {
			t.Fatalf("expected resolve.Unreachable, got %s (%s)", result.Status, result.Detail)
		}
		if !strings.Contains(result.Detail, "fork-network") {
			t.Fatalf("expected detail to mention fork-network, got %q", result.Detail)
		}
		reg.Verify(t)
	})
}

func TestCheckReachability_OnBranch_Reachable(t *testing.T) {
	sha := "abc123abc123abc123abc123abc123abc123abc1"
	reg := &httpmock.Registry{}
	// Fast path: releases/v6 HEAD == sha → exact match → resolve.Reachable.
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/branches`),
		httpmock.JSONResponse(httpmock.BranchListResponse("releases/v6", sha)),
	)

	r, err := resolve.NewWithTransport("github.com", reg)
	if err != nil {
		t.Fatal(err)
	}

	result := New(r).CheckReachability(context.Background(), "actions", "checkout", sha, "v6")
	if result.Status != resolve.Reachable {
		t.Fatalf("expected resolve.Reachable, got %s (%s)", result.Status, result.Detail)
	}
	reg.Verify(t)
}

// TestCheckReachability_ForkInjection_resolve.Unreachable verifies that a fork-network
// commit (not on any branch of the canonical repo) is detected as resolve.Unreachable.
func TestCheckReachability_ForkInjection_Unreachable(t *testing.T) {
	forkSHA := "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"

	reg := &httpmock.Registry{}
	// Empty branch list: fork commits have no branches in the upstream repo.
	probeBranchesEmpty(reg)
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/branches`),
		httpmock.JSONResponse(httpmock.BranchListResponse()),
	)

	r, err := resolve.NewWithTransport("github.com", reg)
	if err != nil {
		t.Fatal(err)
	}

	result := New(r).CheckReachability(context.Background(), "actions", "checkout", forkSHA, "tampered")
	if result.Status != resolve.Unreachable {
		t.Fatalf("expected resolve.Unreachable for fork injection, got %s (%s)", result.Status, result.Detail)
	}
	if !strings.Contains(result.Detail, "fork-network") {
		t.Fatalf("expected detail to mention fork-network, got %q", result.Detail)
	}
	reg.Verify(t)
}

// TestCheckReachability_FoundViaFullScan_SetsFullScanUsed verifies that when a
// commit is not on a canonical branch and is confirmed only after the full
// branch scan, the result flags FullScanUsed so the caller can surface it.
func TestCheckReachability_FoundViaFullScan_SetsFullScanUsed(t *testing.T) {
	sha := "abc123abc123abc123abc123abc123abc123abc1"

	reg := &httpmock.Registry{}
	// Phase 1 protected-branch probe returns empty → canonical set misses.
	probeBranchesEmpty(reg)
	// Phase 2 full listing: a branch whose HEAD differs from sha, forcing the
	// ancestry (Compare) slow path rather than an exact-HEAD match.
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/branches`),
		httpmock.JSONResponse(httpmock.BranchListResponse("feature/x", "fx000")),
	)
	// Compare confirms sha is an ancestor of the branch HEAD → reachable.
	reg.Register(
		httpmock.REST("GET", "repos/actions/checkout/compare/"),
		httpmock.JSONResponse(httpmock.CompareAncestorResponse(sha)),
	)

	r, err := resolve.NewWithTransport("github.com", reg)
	if err != nil {
		t.Fatal(err)
	}

	result := New(r).CheckReachability(context.Background(), "actions", "checkout", sha, "v1")
	if result.Status != resolve.Reachable {
		t.Fatalf("expected resolve.Reachable via full scan, got %s (%s)", result.Status, result.Detail)
	}
	if !result.FullScanUsed {
		t.Fatalf("expected FullScanUsed=true when commit found only via full branch scan")
	}
	reg.Verify(t)
}

// TestCheckReachability_FoundInCanonicalSet_NoFullScan verifies that a commit
// confirmed in the canonical set (here, the default branch fetched directly)
// does NOT flag FullScanUsed.
func TestCheckReachability_FoundInCanonicalSet_NoFullScan(t *testing.T) {
	sha := "aafc3630d7b9aafc3630d7b9aafc3630d7b9aafc"

	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", `repos/vercel/next.js$`),
		httpmock.JSONResponse(map[string]any{"default_branch": "canary"}),
	)
	reg.Register(
		httpmock.REST("GET", `repos/vercel/next.js/git/ref/heads/canary`),
		httpmock.JSONResponse(gitRefHeadResponse("canary", sha)),
	)

	r, err := resolve.NewWithTransport("github.com", reg)
	if err != nil {
		t.Fatal(err)
	}

	result := New(r).CheckReachability(context.Background(), "vercel", "next.js", sha, "canary")
	if result.Status != resolve.Reachable {
		t.Fatalf("expected resolve.Reachable, got %s (%s)", result.Status, result.Detail)
	}
	if result.FullScanUsed {
		t.Fatalf("expected FullScanUsed=false when commit is on a canonical branch")
	}
	reg.Verify(t)
}

// gitRefHeadResponse returns a git/ref response for an exact branch match.
func gitRefHeadResponse(name, sha string) any {
	return map[string]any{
		"ref":    "refs/heads/" + name,
		"object": map[string]any{"sha": sha, "type": "commit"},
	}
}

// probeBranchesEmpty registers HTTP stubs for the phase-1 canonical branch
// probes (getDefaultBranch, listProtectedBranches, listReleaseBranches) for
// actions/checkout, all returning empty results. This forces CheckReachability
// to fall through to the phase-2 full branch scan. Any ref-specific
// getBranchHead lookup that isn't stubbed fails silently (getBranchHead returns
// false on any error), which is correct for the "no canonical branches" case.
func probeBranchesEmpty(reg *httpmock.Registry) {
	// getDefaultBranch: returns empty default so addNamed("") is skipped.
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout$`),
		httpmock.JSONResponse(map[string]any{"default_branch": ""}),
	)
	// listProtectedBranches: empty. RESTWithQuery ensures this stub is only
	// consumed by the ?protected=true call, not by the full listBranches scan.
	reg.Register(
		httpmock.RESTWithQuery("GET", `repos/actions/checkout/branches`, "protected=true"),
		httpmock.JSONResponse([]any{}),
	)
	// listReleaseBranches: matchingHeadRefs("v") and ("release") both empty.
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/git/matching-refs/heads/v`),
		httpmock.JSONResponse([]any{}),
	)
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/git/matching-refs/heads/release`),
		httpmock.JSONResponse([]any{}),
	)
}

// TestCheckReachability_BranchBeyondPageCap_ValidatedViaDirectFetch covers the
// mega-repo case (e.g. vercel/next.js, whose default branch `canary` sorts
// beyond the paginated branch-listing cap). The commit is the HEAD of a branch
// that the full listing would never surface, but the canonical-set phase
// fetches that branch directly via git/ref and confirms reachability — so no
// false impostor verdict. listBranches is intentionally NOT stubbed: a phase-1
// hit must short-circuit before the full scan.
func TestCheckReachability_BranchBeyondPageCap_ValidatedViaDirectFetch(t *testing.T) {
	sha := "aafc3630d7b9aafc3630d7b9aafc3630d7b9aafc"

	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", `repos/vercel/next.js$`),
		httpmock.JSONResponse(map[string]any{"default_branch": "canary"}),
	)
	reg.Register(
		httpmock.REST("GET", `repos/vercel/next.js/git/ref/heads/canary`),
		httpmock.JSONResponse(gitRefHeadResponse("canary", sha)),
	)

	r, err := resolve.NewWithTransport("github.com", reg)
	if err != nil {
		t.Fatal(err)
	}

	result := New(r).CheckReachability(context.Background(), "vercel", "next.js", sha, "canary")
	if result.Status != resolve.Reachable {
		t.Fatalf("expected resolve.Reachable via direct fetch, got %s (%s)", result.Status, result.Detail)
	}
	if !strings.Contains(result.Detail, "canary") {
		t.Fatalf("expected detail to name branch canary, got %q", result.Detail)
	}
	reg.Verify(t)
}

// TestCheckReachability_BranchListError_ReturnsUnknown verifies graceful
// degradation when the branches endpoint fails.
func TestCheckReachability_BranchListError_ReturnsUnknown(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", `repos/actions/checkout/branches`),
		httpmock.StatusResponse(500),
	)

	r, err := resolve.NewWithTransport("github.com", reg)
	if err != nil {
		t.Fatal(err)
	}

	result := New(r).CheckReachability(context.Background(), "actions", "checkout", "abc123abc123abc123abc123abc123abc123abc1", "v6")
	if result.Status != resolve.ReachabilityUnknown {
		t.Fatalf("expected Unknown when branches API fails, got %s (%s)", result.Status, result.Detail)
	}
	reg.Verify(t)
}

func TestCheckAncestry_Confirmed(t *testing.T) {
	pinnedSHA := "abc123abc123abc123abc123abc123abc123abc1"
	liveSHA := "def456def456def456def456def456def456def4"

	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", "repos/actions/checkout/compare/"),
		httpmock.JSONResponse(map[string]any{
			"status": "ahead",
			"merge_base_commit": map[string]any{
				"sha": pinnedSHA,
			},
		}),
	)

	r, err := resolve.NewWithTransport("github.com", reg)
	if err != nil {
		t.Fatal(err)
	}

	status, _ := New(r).CheckAncestry(context.Background(), "actions", "checkout", pinnedSHA, liveSHA)
	if status != resolve.AncestryConfirmed {
		t.Fatalf("expected resolve.AncestryConfirmed, got %d", status)
	}
	reg.Verify(t)
}

func TestCheckAncestry_NotAncestor_DifferentMergeBase(t *testing.T) {
	pinnedSHA := "abc123abc123abc123abc123abc123abc123abc1"
	liveSHA := "def456def456def456def456def456def456def4"

	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", "repos/actions/checkout/compare/"),
		httpmock.JSONResponse(map[string]any{
			"status": "diverged",
			"merge_base_commit": map[string]any{
				"sha": "unrelated_000000000000000000000000000000",
			},
		}),
	)

	r, err := resolve.NewWithTransport("github.com", reg)
	if err != nil {
		t.Fatal(err)
	}

	status, detail := New(r).CheckAncestry(context.Background(), "actions", "checkout", pinnedSHA, liveSHA)
	if status != resolve.AncestryNotAncestor {
		t.Fatalf("expected resolve.AncestryNotAncestor, got %d", status)
	}
	if !strings.Contains(detail, "not the pinned SHA") {
		t.Fatalf("expected detail about merge base mismatch, got %q", detail)
	}
	reg.Verify(t)
}

func TestCheckAncestry_NotAncestor_404(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", "repos/actions/checkout/compare/"),
		httpmock.StatusResponse(404),
	)

	r, err := resolve.NewWithTransport("github.com", reg)
	if err != nil {
		t.Fatal(err)
	}

	status, detail := New(r).CheckAncestry(context.Background(), "actions", "checkout", "abc123abc123abc123abc123abc123abc123abc1", "def456def456def456def456def456def456def4")
	if status != resolve.AncestryNotAncestor {
		t.Fatalf("expected resolve.AncestryNotAncestor for 404, got %d", status)
	}
	if !strings.Contains(detail, "not found") {
		t.Fatalf("expected 'not found' detail, got %q", detail)
	}
	reg.Verify(t)
}

func TestCheckAncestry_NotAncestor_409(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", "repos/actions/checkout/compare/"),
		httpmock.StatusResponse(409),
	)

	r, err := resolve.NewWithTransport("github.com", reg)
	if err != nil {
		t.Fatal(err)
	}

	status, detail := New(r).CheckAncestry(context.Background(), "actions", "checkout", "abc123abc123abc123abc123abc123abc123abc1", "def456def456def456def456def456def456def4")
	if status != resolve.AncestryNotAncestor {
		t.Fatalf("expected resolve.AncestryNotAncestor for 409, got %d", status)
	}
	if !strings.Contains(detail, "no common ancestor") {
		t.Fatalf("expected 'no common ancestor' detail, got %q", detail)
	}
	reg.Verify(t)
}

func TestCheckAncestry_Unknown_RateLimit(t *testing.T) {
	reg := &httpmock.Registry{}
	// Three 429s in a row exhausts ancestryMaxAttempts (=3). The
	// resolver should bottom out at resolve.AncestryUnknown with the
	// retry-budget-exhausted detail rather than treating the first
	// 429 as authoritative.
	for i := 0; i < 3; i++ {
		reg.Register(
			httpmock.REST("GET", "repos/actions/checkout/compare/"),
			httpmock.StatusResponse(429),
		)
	}

	r, err := resolve.NewWithTransport("github.com", reg)
	if err != nil {
		t.Fatal(err)
	}
	r.SetSleepFn(func(context.Context, time.Duration) {})

	status, detail := New(r).CheckAncestry(context.Background(), "actions", "checkout", "abc123abc123abc123abc123abc123abc123abc1", "def456def456def456def456def456def456def4")
	if status != resolve.AncestryUnknown {
		t.Fatalf("expected resolve.AncestryUnknown for rate limit, got %d", status)
	}
	if !strings.Contains(detail, "rate limited") {
		t.Fatalf("expected 'rate limited' detail, got %q", detail)
	}
	if !strings.Contains(detail, "retry budget exhausted") {
		t.Fatalf("expected exhausted-budget detail after three 429s, got %q", detail)
	}
	reg.Verify(t)
}

// TestCheckAncestry_RetrySucceeds documents the happy retry path: two
// transient 429s followed by a 200 must surface resolve.AncestryConfirmed
// without dragging the test through wall-clock backoff.
func TestCheckAncestry_RetrySucceeds(t *testing.T) {
	pinnedSHA := "abc123abc123abc123abc123abc123abc123abc1"
	liveSHA := "def456def456def456def456def456def456def4"

	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", "repos/actions/checkout/compare/"),
		httpmock.StatusResponse(429),
	)
	reg.Register(
		httpmock.REST("GET", "repos/actions/checkout/compare/"),
		httpmock.StatusResponse(429),
	)
	reg.Register(
		httpmock.REST("GET", "repos/actions/checkout/compare/"),
		httpmock.JSONResponse(map[string]any{
			"status":            "ahead",
			"merge_base_commit": map[string]any{"sha": pinnedSHA},
		}),
	)

	r, err := resolve.NewWithTransport("github.com", reg)
	if err != nil {
		t.Fatal(err)
	}
	var sleeps []time.Duration
	r.SetSleepFn(func(_ context.Context, d time.Duration) { sleeps = append(sleeps, d) })

	status, _ := New(r).CheckAncestry(context.Background(), "actions", "checkout", pinnedSHA, liveSHA)
	if status != resolve.AncestryConfirmed {
		t.Fatalf("expected resolve.AncestryConfirmed after two retries, got %d", status)
	}
	if len(sleeps) != 2 {
		t.Fatalf("expected 2 sleeps between 3 attempts, got %d (%v)", len(sleeps), sleeps)
	}
	reg.Verify(t)
}

// TestCheckAncestry_403_RateLimited treats a 403 carrying the
// documented rate-limit headers (X-RateLimit-Reset, Retry-After) the
// same as a 429: retry, then succeed.
func TestCheckAncestry_403_RateLimited(t *testing.T) {
	pinnedSHA := "abc123abc123abc123abc123abc123abc123abc1"
	liveSHA := "def456def456def456def456def456def456def4"

	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", "repos/actions/checkout/compare/"),
		httpmock.StatusResponseWithHeaders(403, map[string]string{
			"X-RateLimit-Remaining": "0",
			"X-RateLimit-Reset":     "1",
		}),
	)
	reg.Register(
		httpmock.REST("GET", "repos/actions/checkout/compare/"),
		httpmock.JSONResponse(map[string]any{
			"status":            "ahead",
			"merge_base_commit": map[string]any{"sha": pinnedSHA},
		}),
	)

	r, err := resolve.NewWithTransport("github.com", reg)
	if err != nil {
		t.Fatal(err)
	}
	r.SetSleepFn(func(context.Context, time.Duration) {})

	status, _ := New(r).CheckAncestry(context.Background(), "actions", "checkout", pinnedSHA, liveSHA)
	if status != resolve.AncestryConfirmed {
		t.Fatalf("expected resolve.AncestryConfirmed after 403 retry, got %d", status)
	}
	reg.Verify(t)
}

// TestCheckAncestry_403_NotRateLimited makes sure a plain 403 (no
// rate-limit headers — the SSO / private-repo / disabled-API shape)
// does not get retried. We register a single stub; a retry would fail
// with "no registered HTTP stubs matched" and surface in the error.
func TestCheckAncestry_403_NotRateLimited(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", "repos/actions/checkout/compare/"),
		httpmock.StatusResponse(403),
	)

	r, err := resolve.NewWithTransport("github.com", reg)
	if err != nil {
		t.Fatal(err)
	}
	slept := false
	r.SetSleepFn(func(context.Context, time.Duration) { slept = true })

	status, detail := New(r).CheckAncestry(context.Background(), "actions", "checkout", "abc123abc123abc123abc123abc123abc123abc1", "def456def456def456def456def456def456def4")
	if status != resolve.AncestryUnknown {
		t.Fatalf("expected resolve.AncestryUnknown for plain 403, got %d", status)
	}
	if slept {
		t.Fatalf("plain 403 must not retry; got a sleep call")
	}
	if strings.Contains(detail, "rate limited") {
		t.Fatalf("plain 403 must not be reported as rate limited, got %q", detail)
	}
	reg.Verify(t)
}

// TestCheckAncestry_RateLimitResetBeyondBudget bails immediately when
// X-RateLimit-Reset is so far in the future that sleeping to honor it
// would blow the retry budget. Avoids pointlessly stalling a doctor run
// for the full budget when the call would still fail.
func TestCheckAncestry_RateLimitResetBeyondBudget(t *testing.T) {
	reg := &httpmock.Registry{}
	farFutureReset := strconv.FormatInt(time.Now().Add(2*time.Hour).Unix(), 10)
	reg.Register(
		httpmock.REST("GET", "repos/actions/checkout/compare/"),
		httpmock.StatusResponseWithHeaders(429, map[string]string{
			"X-RateLimit-Reset": farFutureReset,
		}),
	)

	r, err := resolve.NewWithTransport("github.com", reg)
	if err != nil {
		t.Fatal(err)
	}
	slept := false
	r.SetSleepFn(func(context.Context, time.Duration) { slept = true })

	status, detail := New(r).CheckAncestry(context.Background(), "actions", "checkout", "abc123abc123abc123abc123abc123abc123abc1", "def456def456def456def456def456def456def4")
	if status != resolve.AncestryUnknown {
		t.Fatalf("expected resolve.AncestryUnknown when reset is beyond budget, got %d", status)
	}
	if slept {
		t.Fatalf("must not sleep when reset is beyond budget")
	}
	if !strings.Contains(detail, "budget") {
		t.Fatalf("expected budget-exceeded detail, got %q", detail)
	}
	reg.Verify(t)
}
