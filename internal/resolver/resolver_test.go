package resolver

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/github/gh-actions-pin/internal/httpmock"
	"github.com/github/gh-actions-pin/internal/lockfile"
)

func TestSelectLatestTagPrefersHighestMajorTag(t *testing.T) {
	got := selectLatestTag([]string{"v3", "v6", "v5", "v4"})
	if got != "v6" {
		t.Fatalf("expected latest major tag v6, got %q", got)
	}
}

func TestSelectLatestTagFallsBackToHighestStableSemver(t *testing.T) {
	got := selectLatestTag([]string{"v1.2.0", "v1.10.0", "v1.9.9"})
	if got != "v1.10.0" {
		t.Fatalf("expected latest semver tag v1.10.0, got %q", got)
	}
}

func TestSelectLatestTagFallbacksWhenNoStableSemver(t *testing.T) {
	got := selectLatestTag([]string{"main", "nightly", "release-candidate"})
	if got != "release-candidate" {
		t.Fatalf("expected lexical fallback tag, got %q", got)
	}
}

func TestBuildResolveWithFileQuery(t *testing.T) {
	refs := []lockfile.ActionRef{
		{Owner: "actions", Repo: "checkout", Ref: "v6"},
		{Owner: "actions", Repo: "cache", Path: "save", Ref: "v4"},
	}

	query, aliases := buildResolveWithFileQuery(refs)
	if len(aliases) != 2 {
		t.Fatalf("expected two aliases, got %+v", aliases)
	}
	for _, want := range []string{
		`a0: repository(owner: "actions", name: "checkout")`,
		`object(expression: "v6")`,
		`file(path: "action.yml")`,
		`a1: repository(owner: "actions", name: "cache")`,
		`file(path: "save/action.yml")`,
		`fileYaml: file(path: "save/action.yaml")`,
	} {
		if !strings.Contains(query, want) {
			t.Fatalf("query missing %q:\n%s", want, query)
		}
	}
}

func TestParseResolveWithFileResponse(t *testing.T) {
	refs := []lockfile.ActionRef{
		{Owner: "actions", Repo: "checkout", Ref: "v6"},
		{Owner: "actions", Repo: "cache", Path: "save", Ref: "v4"},
	}
	aliases := map[string]int{"a0": 0, "a1": 1}

	data := map[string]json.RawMessage{
		"a0": json.RawMessage(`{"nameWithOwner":"actions/checkout","object":{"oid":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","file":{"object":{"text":"name: Checkout\nruns:\n  using: node20\n"}}}}`),
		"a1": json.RawMessage(`{"nameWithOwner":"actions/cache","object":{"oid":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","fileYaml":{"object":{"text":"name: Cache Save\nruns:\n  using: composite\n  steps:\n    - uses: actions/upload-artifact@v4\n"}}}}`),
	}

	deps, ymls, err := parseResolveWithFileResponse(data, refs, aliases)
	if err != nil {
		t.Fatalf("parseResolveWithFileResponse returned error: %v", err)
	}
	if len(deps) != 2 || len(ymls) != 2 {
		t.Fatalf("expected two deps/ymls, got %d and %d", len(deps), len(ymls))
	}
	keys := map[string]bool{}
	for _, dep := range deps {
		keys[dep.Key()] = true
	}
	if !keys["actions/checkout@v6"] || !keys["actions/cache/save@v4"] {
		t.Fatalf("unexpected deps: %+v", deps)
	}
	foundFallback := false
	for _, yml := range ymls {
		if strings.Contains(yml, "upload-artifact") {
			foundFallback = true
			break
		}
	}
	if !foundFallback {
		t.Fatalf("expected yaml content from fileYaml fallback, got %#v", ymls)
	}
}

func TestParseResolveWithFileResponseErrors(t *testing.T) {
	refs := []lockfile.ActionRef{
		{Owner: "actions", Repo: "checkout", Ref: "v6"},
		{Owner: "actions", Repo: "setup-go", Ref: "v6"},
		{Owner: "actions", Repo: "cache", Ref: "v4"},
	}
	aliases := map[string]int{"a0": 0, "a1": 1, "a2": 2}
	data := map[string]json.RawMessage{
		"a0": json.RawMessage(`null`),
		"a1": json.RawMessage(`{"nameWithOwner":"actions/setup-go","object":{"oid":""}}`),
	}

	_, _, err := parseResolveWithFileResponse(data, refs, aliases)
	if err == nil {
		t.Fatal("expected parseResolveWithFileResponse to return aggregated errors")
	}
	for _, want := range []string{
		"actions/checkout@v6: repository not found or not accessible",
		`actions/setup-go@v6: ref "v6" does not exist`,
		"actions/cache@v4: not found in response",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("expected error to contain %q, got %v", want, err)
		}
	}
}

func TestResolveAllRecursiveWithCacheAndCompositeExpansion(t *testing.T) {
	r := &Resolver{
		MaxRecursionDepth: DefaultMaxRecursionDepth,
		cache: map[string]resolvedEntry{
			"actions/checkout@v6": {
				dep: lockfile.Dependency{
					NWO: "actions/checkout", Ref: "v6", SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				},
				actionYML: "name: Checkout\nruns:\n  using: node20\n",
			},
		},
		latestRefCache: map[string]string{},
		reachCache:     map[string]ReachabilityStatus{},
	}

	r.cache["owner/composite@v1"] = resolvedEntry{
		dep: lockfile.Dependency{
			NWO: "owner/composite", Ref: "v1", SHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		},
		actionYML: "name: Composite\nruns:\n  using: composite\n  steps:\n    - uses: actions/setup-go@v6\n",
	}
	r.cache["actions/setup-go@v6"] = resolvedEntry{
		dep: lockfile.Dependency{
			NWO: "actions/setup-go", Ref: "v6", SHA: "cccccccccccccccccccccccccccccccccccccccc",
		},
		actionYML: "name: Setup Go\nruns:\n  using: node20\n",
	}

	deps, err := r.ResolveAllRecursive([]lockfile.ActionRef{
		{Owner: "actions", Repo: "checkout", Ref: "v6"},
		{Owner: "owner", Repo: "composite", Ref: "v1"},
	})
	if err != nil {
		t.Fatalf("ResolveAllRecursive returned error: %v", err)
	}

	if len(deps) != 3 {
		t.Fatalf("expected three unique deps, got %d: %+v", len(deps), deps)
	}

	// Verify parentMap tracks the child dep key → parent dep key.
	pm := r.ParentMap()
	parents, ok := pm["actions/setup-go@v6"]
	if !ok || len(parents) != 1 || parents[0] != "owner/composite@v1" {
		t.Fatalf("expected parentMap to map actions/setup-go@v6 → [owner/composite@v1], got %v", pm)
	}
}

func TestResolveAllRecursiveMultipleParents(t *testing.T) {
	r := &Resolver{
		MaxRecursionDepth: DefaultMaxRecursionDepth,
		cache: map[string]resolvedEntry{
			"owner/compositeA@v1": {
				dep:       lockfile.Dependency{NWO: "owner/compositeA", Ref: "v1", SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
				actionYML: "name: A\nruns:\n  using: composite\n  steps:\n    - uses: shared/dep@v1\n",
			},
			"owner/compositeB@v1": {
				dep:       lockfile.Dependency{NWO: "owner/compositeB", Ref: "v1", SHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
				actionYML: "name: B\nruns:\n  using: composite\n  steps:\n    - uses: shared/dep@v1\n",
			},
			"shared/dep@v1": {
				dep:       lockfile.Dependency{NWO: "shared/dep", Ref: "v1", SHA: "cccccccccccccccccccccccccccccccccccccccc"},
				actionYML: "name: Shared\nruns:\n  using: node20\n",
			},
		},
		latestRefCache: map[string]string{},
		reachCache:     map[string]ReachabilityStatus{},
	}

	deps, err := r.ResolveAllRecursive([]lockfile.ActionRef{
		{Owner: "owner", Repo: "compositeA", Ref: "v1"},
		{Owner: "owner", Repo: "compositeB", Ref: "v1"},
	})
	if err != nil {
		t.Fatalf("ResolveAllRecursive returned error: %v", err)
	}

	if len(deps) != 3 {
		t.Fatalf("expected 3 unique deps, got %d: %+v", len(deps), deps)
	}

	pm := r.ParentMap()
	parents, ok := pm["shared/dep@v1"]
	if !ok {
		t.Fatal("expected parentMap to contain shared/dep@v1")
	}
	if len(parents) != 2 {
		t.Fatalf("expected 2 parents for shared/dep@v1, got %d: %v", len(parents), parents)
	}
	// Both composites should be parents (order may vary).
	hasA, hasB := false, false
	for _, p := range parents {
		if p == "owner/compositeA@v1" {
			hasA = true
		}
		if p == "owner/compositeB@v1" {
			hasB = true
		}
	}
	if !hasA || !hasB {
		t.Fatalf("expected both compositeA and compositeB as parents, got %v", parents)
	}
}

func TestResolveAllRecursiveRespectsMaxDepth(t *testing.T) {
	r := &Resolver{
		MaxRecursionDepth: 1,
		cache: map[string]resolvedEntry{
			"owner/composite@v1": {
				dep:       lockfile.Dependency{NWO: "owner/composite", Ref: "v1", SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
				actionYML: "name: Composite\nruns:\n  using: composite\n  steps:\n    - uses: owner/nested@v1\n",
			},
			"owner/nested@v1": {
				dep:       lockfile.Dependency{NWO: "owner/nested", Ref: "v1", SHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
				actionYML: "name: Nested\nruns:\n  using: composite\n  steps:\n    - uses: actions/checkout@v6\n",
			},
		},
		latestRefCache: map[string]string{},
		reachCache:     map[string]ReachabilityStatus{},
	}

	_, err := r.ResolveAllRecursive([]lockfile.ActionRef{{Owner: "owner", Repo: "composite", Ref: "v1"}})
	if err == nil || !strings.Contains(err.Error(), "exceeded max depth 1") {
		t.Fatalf("expected recursion depth error, got %v", err)
	}
}

func TestNewWithTransportAndLatestRef(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	reg.Register(
		httpmock.GraphQL(`refs\(refPrefix: "refs/tags/"`),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"repository": map[string]any{
					"refs": map[string]any{
						"nodes": []map[string]string{
							{"name": "v4"},
							{"name": "v5"},
							{"name": "v6"},
						},
					},
				},
			},
		}),
	)

	r, err := NewWithTransport("github.com", reg)
	if err != nil {
		t.Fatalf("NewWithTransport returned error: %v", err)
	}
	if host := r.Hostname(); host != "github.com" {
		t.Fatalf("expected hostname github.com, got %q", host)
	}

	ref, err := r.LatestRef("actions", "checkout")
	if err != nil {
		t.Fatalf("LatestRef returned error: %v", err)
	}
	if ref != "v6" {
		t.Fatalf("expected latest ref v6, got %q", ref)
	}

	// Second call should hit the cache instead of requiring another stub.
	ref, err = r.LatestRef("actions", "checkout")
	if err != nil {
		t.Fatalf("LatestRef cache lookup returned error: %v", err)
	}
	if ref != "v6" {
		t.Fatalf("expected cached latest ref v6, got %q", ref)
	}
}

func TestResolveAllRecursiveWithHTTPTransport(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	reg.Register(
		httpmock.GraphQL(`repository\(owner: "owner", name: "composite"\)`),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": map[string]any{
					"nameWithOwner": "owner/composite",
					"object": map[string]any{
						"oid": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
						"file": map[string]any{
							"object": map[string]any{
								"text": "name: Composite\nruns:\n  using: composite\n  steps:\n    - uses: actions/checkout@v6\n",
							},
						},
					},
				},
			},
		}),
	)
	reg.Register(
		httpmock.GraphQL(`repository\(owner: "actions", name: "checkout"\)`),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": map[string]any{
					"nameWithOwner": "actions/checkout",
					"object": map[string]any{
						"oid": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
						"file": map[string]any{
							"object": map[string]any{
								"text": "name: Checkout\nruns:\n  using: node20\n",
							},
						},
					},
				},
			},
		}),
	)

	r, err := NewWithTransport("github.com", reg)
	if err != nil {
		t.Fatalf("NewWithTransport returned error: %v", err)
	}

	deps, err := r.ResolveAllRecursive([]lockfile.ActionRef{{Owner: "owner", Repo: "composite", Ref: "v1"}})
	if err != nil {
		t.Fatalf("ResolveAllRecursive returned error: %v", err)
	}
	if len(deps) != 2 {
		t.Fatalf("expected two deps, got %d: %+v", len(deps), deps)
	}
	if deps[0].NWO != "owner/composite" && deps[1].NWO != "owner/composite" {
		t.Fatalf("expected composite dep to be present, got %+v", deps)
	}
}

func TestCheckReachability_Reachable(t *testing.T) {
	r := &Resolver{
		reachCache: map[string]ReachabilityStatus{},
		checkReachFn: func(owner, repo, sha, ref string) (ReachabilityStatus, string) {
			return Reachable, "ancestor of " + ref
		},
	}
	result := r.CheckReachability("actions", "checkout", "abc123", "v6")
	if result.Status != Reachable {
		t.Fatalf("expected Reachable, got %s (%s)", result.Status, result.Detail)
	}
}

func TestCheckReachability_Unreachable(t *testing.T) {
	r := &Resolver{
		reachCache: map[string]ReachabilityStatus{},
		checkReachFn: func(owner, repo, sha, ref string) (ReachabilityStatus, string) {
			return Unreachable, "commit is not an ancestor of " + ref
		},
	}
	result := r.CheckReachability("evil", "repo", "deadbeef", "v1")
	if result.Status != Unreachable {
		t.Fatalf("expected Unreachable, got %s (%s)", result.Status, result.Detail)
	}
}

func TestCheckReachability_Unknown(t *testing.T) {
	r := &Resolver{
		reachCache: map[string]ReachabilityStatus{},
		checkReachFn: func(owner, repo, sha, ref string) (ReachabilityStatus, string) {
			return ReachabilityUnknown, "clone failed"
		},
	}
	result := r.CheckReachability("actions", "checkout", "abc123", "v6")
	if result.Status != ReachabilityUnknown {
		t.Fatalf("expected Unknown, got %s (%s)", result.Status, result.Detail)
	}
}

func TestCheckReachability_CachesResults(t *testing.T) {
	calls := 0
	r := &Resolver{
		reachCache: map[string]ReachabilityStatus{},
		checkReachFn: func(owner, repo, sha, ref string) (ReachabilityStatus, string) {
			calls++
			return Reachable, "ancestor of " + ref
		},
	}

	r1 := r.CheckReachability("actions", "checkout", "abc123", "v6")
	r2 := r.CheckReachability("actions", "checkout", "abc123", "v6")

	if r1.Status != Reachable || r2.Status != Reachable {
		t.Fatalf("expected both calls to return Reachable, got %s and %s", r1.Status, r2.Status)
	}
	if r2.Detail != "cached" {
		t.Fatalf("expected second call to be cached, got detail %q", r2.Detail)
	}
	if calls != 1 {
		t.Fatalf("expected checkReachFn called once, got %d", calls)
	}
}

func TestCheckReachabilityAll_DeduplicatesRequests(t *testing.T) {
	calls := 0
	r := &Resolver{
		reachCache: map[string]ReachabilityStatus{},
		checkReachFn: func(owner, repo, sha, ref string) (ReachabilityStatus, string) {
			calls++
			return Reachable, "ancestor of " + ref
		},
	}

	deps := []lockfile.Dependency{
		{NWO: "actions/checkout", Ref: "v6", SHA: "aaa"},
		{NWO: "actions/checkout", Ref: "v6", SHA: "aaa"}, // duplicate
		{NWO: "actions/setup-go", Ref: "v6", SHA: "bbb"},
	}

	results := r.CheckReachabilityAll(deps)
	if len(results) != 2 {
		t.Fatalf("expected 2 unique results, got %d: %+v", len(results), results)
	}
	if calls != 2 {
		t.Fatalf("expected 2 calls (deduped), got %d", calls)
	}
}

func TestCheckReachability_SHAAsRef_ChecksDefaultBranch(t *testing.T) {
	sha := "abc123abc123abc123abc123abc123abc123abc1"

	t.Run("reachable from HEAD", func(t *testing.T) {
		reg := &httpmock.Registry{}
		reg.Register(
			httpmock.REST("GET", "repos/actions/checkout/compare/"),
			httpmock.JSONResponse(map[string]any{
				"status": "ahead",
				"merge_base_commit": map[string]any{
					"sha": sha,
				},
			}),
		)
		r, err := NewWithTransport("github.com", reg)
		if err != nil {
			t.Fatal(err)
		}
		result := r.CheckReachability("actions", "checkout", sha, sha)
		if result.Status != Reachable {
			t.Fatalf("expected Reachable, got %s (%s)", result.Status, result.Detail)
		}
		if !strings.Contains(result.Detail, "bare SHA") {
			t.Fatalf("expected detail to mention bare SHA, got %q", result.Detail)
		}
		reg.Verify(t)
	})

	t.Run("unreachable from HEAD", func(t *testing.T) {
		reg := &httpmock.Registry{}
		reg.Register(
			httpmock.REST("GET", "repos/actions/checkout/compare/"),
			httpmock.JSONResponse(map[string]any{
				"status": "diverged",
				"merge_base_commit": map[string]any{
					"sha": "different_sha_000000000000000000000000000",
				},
			}),
		)
		r, err := NewWithTransport("github.com", reg)
		if err != nil {
			t.Fatal(err)
		}
		result := r.CheckReachability("actions", "checkout", sha, sha)
		if result.Status != Unreachable {
			t.Fatalf("expected Unreachable, got %s (%s)", result.Status, result.Detail)
		}
		if !strings.Contains(result.Detail, "fork-network") {
			t.Fatalf("expected detail to mention fork-network, got %q", result.Detail)
		}
		reg.Verify(t)
	})
}

func TestApiReachabilityCheck_Reachable(t *testing.T) {
	reg := &httpmock.Registry{}
	// First call: ancestry check (compare/sha...ref) — merge_base matches pinned SHA
	reg.Register(
		httpmock.REST("GET", "repos/actions/checkout/compare/abc123"),
		httpmock.JSONResponse(map[string]any{
			"status": "ahead",
			"merge_base_commit": map[string]any{
				"sha": "abc123abc123abc123abc123abc123abc123abc1",
			},
		}),
	)
	// Second call: resolve ref to SHA
	reg.Register(
		httpmock.REST("GET", "repos/actions/checkout/commits/v6"),
		httpmock.JSONResponse(map[string]any{
			"sha": "def456def456def456def456def456def456def4",
		}),
	)
	// Third call: branch_commits — commit is on a branch
	reg.Register(
		httpmock.REST("GET", "actions/checkout/branch_commits/def456"),
		httpmock.JSONResponse(map[string]any{
			"branches": []map[string]any{
				{"branch": "main", "prs": []any{}},
			},
			"tags": []string{"v6"},
		}),
	)

	r, err := NewWithTransport("github.com", reg)
	if err != nil {
		t.Fatal(err)
	}

	result := r.CheckReachability("actions", "checkout", "abc123abc123abc123abc123abc123abc123abc1", "v6")
	if result.Status != Reachable {
		t.Fatalf("expected Reachable, got %s (%s)", result.Status, result.Detail)
	}
	reg.Verify(t)
}

func TestApiReachabilityCheck_Unreachable_ForkCommit(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", "repos/actions/checkout/compare/"),
		httpmock.JSONResponse(map[string]any{
			"status": "behind",
			"merge_base_commit": map[string]any{
				"sha": "different_sha_000000000000000000000000000",
			},
		}),
	)

	r, err := NewWithTransport("github.com", reg)
	if err != nil {
		t.Fatal(err)
	}

	result := r.CheckReachability("actions", "checkout", "abc123abc123abc123abc123abc123abc123abc1", "v6")
	if result.Status != Unreachable {
		t.Fatalf("expected Unreachable, got %s (%s)", result.Status, result.Detail)
	}
	if !strings.Contains(result.Detail, "fork-network") {
		t.Fatalf("expected detail to mention fork-network, got %q", result.Detail)
	}
	reg.Verify(t)
}

func TestApiReachabilityCheck_Unreachable_404(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", "repos/actions/checkout/compare/"),
		httpmock.StatusResponse(404),
	)

	r, err := NewWithTransport("github.com", reg)
	if err != nil {
		t.Fatal(err)
	}

	result := r.CheckReachability("actions", "checkout", "abc123abc123abc123abc123abc123abc123abc1", "v6")
	if result.Status != Unreachable {
		t.Fatalf("expected Unreachable for 404, got %s (%s)", result.Status, result.Detail)
	}
	reg.Verify(t)
}

func TestApiReachabilityCheck_Unknown_RateLimit(t *testing.T) {
	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", "repos/actions/checkout/compare/"),
		httpmock.StatusResponse(429),
	)

	r, err := NewWithTransport("github.com", reg)
	if err != nil {
		t.Fatal(err)
	}

	result := r.CheckReachability("actions", "checkout", "abc123abc123abc123abc123abc123abc123abc1", "v6")
	if result.Status != ReachabilityUnknown {
		t.Fatalf("expected Unknown for rate limit, got %s (%s)", result.Status, result.Detail)
	}
	if !strings.Contains(result.Detail, "rate limited") {
		t.Fatalf("expected detail to mention rate limit, got %q", result.Detail)
	}
	reg.Verify(t)
}

// TestApiReachabilityCheck_ForkInjection_PreservedLineage tests the critical
// scenario where a tag has been moved to point at a fork-network commit that
// preserves lineage (its parent is a real upstream commit). The ancestry check
// passes (merge_base == pinnedSHA) but the branch_commits check catches it
// because the fork commit is not on any branch in the upstream repo.
func TestApiReachabilityCheck_ForkInjection_PreservedLineage(t *testing.T) {
	pinnedSHA := "abc123abc123abc123abc123abc123abc123abc1"
	forkSHA := "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"

	reg := &httpmock.Registry{}
	// Ancestry check: compare(pinnedSHA...tampered) → merge_base matches
	// because the fork commit descends from pinnedSHA through the fork network
	reg.Register(
		httpmock.REST("GET", "repos/actions/checkout/compare/abc123"),
		httpmock.JSONResponse(map[string]any{
			"status": "ahead",
			"merge_base_commit": map[string]any{
				"sha": pinnedSHA,
			},
		}),
	)
	// Resolve ref to the fork commit SHA
	reg.Register(
		httpmock.REST("GET", "repos/actions/checkout/commits/tampered"),
		httpmock.JSONResponse(map[string]any{
			"sha": forkSHA,
		}),
	)
	// branch_commits: fork commit has NO branches (only the tampered tag)
	reg.Register(
		httpmock.REST("GET", "actions/checkout/branch_commits/deadbeefdead"),
		httpmock.JSONResponse(map[string]any{
			"branches": []any{},
			"tags":     []string{"tampered"},
		}),
	)

	r, err := NewWithTransport("github.com", reg)
	if err != nil {
		t.Fatal(err)
	}

	result := r.CheckReachability("actions", "checkout", pinnedSHA, "tampered")
	if result.Status != Unreachable {
		t.Fatalf("expected Unreachable for fork injection with preserved lineage, got %s (%s)", result.Status, result.Detail)
	}
	if !strings.Contains(result.Detail, "fork-network") {
		t.Fatalf("expected detail to mention fork-network, got %q", result.Detail)
	}
	reg.Verify(t)
}

// TestApiReachabilityCheck_BranchCommitsUnknown_KeepsReachable verifies that when
// the ancestry check passes but the branch_commits check returns Unknown (e.g.
// API failure), we keep the Reachable result rather than downgrading.
func TestApiReachabilityCheck_BranchCommitsUnknown_KeepsReachable(t *testing.T) {
	pinnedSHA := "abc123abc123abc123abc123abc123abc123abc1"

	reg := &httpmock.Registry{}
	// Ancestry check passes
	reg.Register(
		httpmock.REST("GET", "repos/actions/checkout/compare/abc123"),
		httpmock.JSONResponse(map[string]any{
			"status": "ahead",
			"merge_base_commit": map[string]any{
				"sha": pinnedSHA,
			},
		}),
	)
	// Resolve ref to SHA
	reg.Register(
		httpmock.REST("GET", "repos/actions/checkout/commits/v6"),
		httpmock.JSONResponse(map[string]any{
			"sha": "def456def456def456def456def456def456def4",
		}),
	)
	// branch_commits fails (e.g. 500)
	reg.Register(
		httpmock.REST("GET", "actions/checkout/branch_commits/def456"),
		httpmock.StatusResponse(500),
	)

	r, err := NewWithTransport("github.com", reg)
	if err != nil {
		t.Fatal(err)
	}

	result := r.CheckReachability("actions", "checkout", pinnedSHA, "v6")
	if result.Status != Reachable {
		t.Fatalf("expected Reachable when branch_commits check is Unknown, got %s (%s)", result.Status, result.Detail)
	}
	reg.Verify(t)
}
