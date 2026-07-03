package resolve

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/dep"
	"github.com/github/gh-actions-lock/internal/ghapi"
	"github.com/github/gh-actions-lock/internal/ghapi/httpmock"
	"github.com/github/gh-actions-lock/internal/pinpool"
)

// seedCache populates r.cache with the supplied entries and returns r so it can
// be used inline with a struct literal. It exists so test setup stays close to
// pre-refactor readability while honoring the syncMap-backed cache fields.
func seedCache(r *Resolver, m map[ghapi.ActionRef]resolvedEntry) *Resolver {
	for k, v := range m {
		r.cache.Put(k, v)
	}
	return r
}

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

func TestResolveAllRecursiveWithCacheAndCompositeExpansion(t *testing.T) {
	r := seedCache(&Resolver{
		MaxRecursionDepth: DefaultMaxRecursionDepth,
	}, map[ghapi.ActionRef]resolvedEntry{
		ghapi.ForActionRef("actions", "checkout", "", "v6"): {
			dep: dep.Dependency{
				NWO: "actions/checkout", Ref: "v6", SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			},
			actionYML: "name: Checkout\nruns:\n  using: node20\n",
		},
	})

	r.cache.Put(ghapi.ForActionRef("owner", "composite", "", "v1"), resolvedEntry{
		dep: dep.Dependency{
			NWO: "owner/composite", Ref: "v1", SHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		},
		actionYML: "name: Composite\nruns:\n  using: composite\n  steps:\n    - uses: actions/setup-go@v6\n",
	})
	r.cache.Put(ghapi.ForActionRef("actions", "setup-go", "", "v6"), resolvedEntry{
		dep: dep.Dependency{
			NWO: "actions/setup-go", Ref: "v6", SHA: "cccccccccccccccccccccccccccccccccccccccc",
		},
		actionYML: "name: Setup Go\nruns:\n  using: node20\n",
	})

	deps, parentMapForTest, err := r.ResolveAllRecursive(context.Background(), []parserlock.ActionRef{
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
	pm := parentMapForTest
	parents, ok := pm["actions/setup-go@v6"]
	if !ok || len(parents) != 1 || parents[0] != "owner/composite@v1" {
		t.Fatalf("expected parentMap to map actions/setup-go@v6 → [owner/composite@v1], got %v", pm)
	}
}

func TestResolveAllRecursiveMultipleParents(t *testing.T) {
	r := seedCache(&Resolver{
		MaxRecursionDepth: DefaultMaxRecursionDepth,
	}, map[ghapi.ActionRef]resolvedEntry{
		ghapi.ForActionRef("owner", "compositeA", "", "v1"): {
			dep:       dep.Dependency{NWO: "owner/compositeA", Ref: "v1", SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
			actionYML: "name: A\nruns:\n  using: composite\n  steps:\n    - uses: shared/dep@v1\n",
		},
		ghapi.ForActionRef("owner", "compositeB", "", "v1"): {
			dep:       dep.Dependency{NWO: "owner/compositeB", Ref: "v1", SHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
			actionYML: "name: B\nruns:\n  using: composite\n  steps:\n    - uses: shared/dep@v1\n",
		},
		ghapi.ForActionRef("shared", "dep", "", "v1"): {
			dep:       dep.Dependency{NWO: "shared/dep", Ref: "v1", SHA: "cccccccccccccccccccccccccccccccccccccccc"},
			actionYML: "name: Shared\nruns:\n  using: node20\n",
		},
	})

	deps, parentMapForTest, err := r.ResolveAllRecursive(context.Background(), []parserlock.ActionRef{
		{Owner: "owner", Repo: "compositeA", Ref: "v1"},
		{Owner: "owner", Repo: "compositeB", Ref: "v1"},
	})
	if err != nil {
		t.Fatalf("ResolveAllRecursive returned error: %v", err)
	}

	if len(deps) != 3 {
		t.Fatalf("expected 3 unique deps, got %d: %+v", len(deps), deps)
	}

	pm := parentMapForTest
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
	r := seedCache(&Resolver{
		MaxRecursionDepth: 1,
	}, map[ghapi.ActionRef]resolvedEntry{
		ghapi.ForActionRef("owner", "composite", "", "v1"): {
			dep:       dep.Dependency{NWO: "owner/composite", Ref: "v1", SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
			actionYML: "name: Composite\nruns:\n  using: composite\n  steps:\n    - uses: owner/nested@v1\n",
		},
		ghapi.ForActionRef("owner", "nested", "", "v1"): {
			dep:       dep.Dependency{NWO: "owner/nested", Ref: "v1", SHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
			actionYML: "name: Nested\nruns:\n  using: composite\n  steps:\n    - uses: actions/checkout@v6\n",
		},
	})

	_, _, err := r.ResolveAllRecursive(context.Background(), []parserlock.ActionRef{{Owner: "owner", Repo: "composite", Ref: "v1"}})
	if err == nil || !strings.Contains(err.Error(), "exceeded max depth 1") {
		t.Fatalf("expected recursion depth error, got %v", err)
	}
}

// TestResolveAllRecursiveDeepNestedComposites verifies a 3-level chain
// A → B → C produces a properly threaded parentMap so the lockfile writer
// can emit `uses:` at every level.
func TestResolveAllRecursiveDeepNestedComposites(t *testing.T) {
	r := seedCache(&Resolver{
		MaxRecursionDepth: DefaultMaxRecursionDepth,
	}, map[ghapi.ActionRef]resolvedEntry{
		ghapi.ForActionRef("owner", "a", "", "v1"): {
			dep:       dep.Dependency{NWO: "owner/a", Ref: "v1", SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
			actionYML: "name: A\nruns:\n  using: composite\n  steps:\n    - uses: owner/b@v1\n",
		},
		ghapi.ForActionRef("owner", "b", "", "v1"): {
			dep:       dep.Dependency{NWO: "owner/b", Ref: "v1", SHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
			actionYML: "name: B\nruns:\n  using: composite\n  steps:\n    - uses: owner/c@v1\n",
		},
		ghapi.ForActionRef("owner", "c", "", "v1"): {
			dep:       dep.Dependency{NWO: "owner/c", Ref: "v1", SHA: "cccccccccccccccccccccccccccccccccccccccc"},
			actionYML: "name: C\nruns:\n  using: node20\n",
		},
	})

	deps, parentMapForTest, err := r.ResolveAllRecursive(context.Background(), []parserlock.ActionRef{{Owner: "owner", Repo: "a", Ref: "v1"}})
	if err != nil {
		t.Fatalf("ResolveAllRecursive returned error: %v", err)
	}
	if len(deps) != 3 {
		t.Fatalf("expected 3 deps, got %d: %+v", len(deps), deps)
	}

	pm := parentMapForTest
	if got := pm["owner/b@v1"]; len(got) != 1 || got[0] != "owner/a@v1" {
		t.Errorf("expected owner/b@v1 parent = [owner/a@v1], got %v", got)
	}
	if got := pm["owner/c@v1"]; len(got) != 1 || got[0] != "owner/b@v1" {
		t.Errorf("expected owner/c@v1 parent = [owner/b@v1], got %v", got)
	}
	// Root has no parents (workflow-direct).
	if _, ok := pm["owner/a@v1"]; ok {
		t.Errorf("expected owner/a@v1 to have no parents, got %v", pm["owner/a@v1"])
	}
}

// TestResolveAllRecursiveSkipsSelfReference verifies that a composite action
// whose `uses:` names its own host repo+ref (a same-tarball routing concern)
// is not recorded as its own transitive dependency.
func TestResolveAllRecursiveSkipsSelfReference(t *testing.T) {
	r := seedCache(&Resolver{
		MaxRecursionDepth: DefaultMaxRecursionDepth,
	}, map[ghapi.ActionRef]resolvedEntry{
		ghapi.ForActionRef("owner", "repo", "", "main"): {
			dep: dep.Dependency{NWO: "owner/repo", Ref: "main", SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
			// Self-ref: composite's uses points back at its own NWO@Ref.
			actionYML: "name: Self\nruns:\n  using: composite\n  steps:\n    - uses: owner/repo@main\n",
		},
	})

	deps, parentMapForTest, err := r.ResolveAllRecursive(context.Background(), []parserlock.ActionRef{{Owner: "owner", Repo: "repo", Ref: "main"}})
	if err != nil {
		t.Fatalf("ResolveAllRecursive returned error: %v", err)
	}
	if len(deps) != 1 {
		t.Fatalf("expected single dep (no self-loop expansion), got %d: %+v", len(deps), deps)
	}
	pm := parentMapForTest
	if parents, ok := pm["owner/repo@main"]; ok {
		t.Errorf("expected no self-parent for owner/repo@main, got %v", parents)
	}
}

// TestResolveAllRecursiveSiblingSubpathTransitive verifies that a composite
// whose `uses:` names a SIBLING subpath in its own repo+ref (a same-tarball
// edge) is still traversed for its cross-repo transitive deps. Repo layout:
//
//	org/fixtures/nested-composite  (main)  -- uses -->
//	org/fixtures/simple-composite  (main, same tarball) -- uses -->
//	org/fixtures-b                 (main, different repo)
//
// The same-tarball edge nested→simple must not be pruned, otherwise the
// 2-levels-deep org/fixtures-b transitive dep is never discovered.
func TestResolveAllRecursiveSiblingSubpathTransitive(t *testing.T) {
	const tarballSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	r := seedCache(&Resolver{
		MaxRecursionDepth: DefaultMaxRecursionDepth,
	}, map[ghapi.ActionRef]resolvedEntry{
		ghapi.ForActionRef("org", "fixtures", "nested-composite", "main"): {
			dep:       dep.Dependency{NWO: "org/fixtures", Path: "nested-composite", Ref: "main", SHA: tarballSHA},
			actionYML: "name: Nested\nruns:\n  using: composite\n  steps:\n    - uses: org/fixtures/simple-composite@main\n",
		},
		ghapi.ForActionRef("org", "fixtures", "simple-composite", "main"): {
			dep:       dep.Dependency{NWO: "org/fixtures", Path: "simple-composite", Ref: "main", SHA: tarballSHA},
			actionYML: "name: Simple\nruns:\n  using: composite\n  steps:\n    - uses: org/fixtures-b@main\n",
		},
		ghapi.ForActionRef("org", "fixtures-b", "", "main"): {
			dep:       dep.Dependency{NWO: "org/fixtures-b", Ref: "main", SHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
			actionYML: "name: B\nruns:\n  using: node20\n",
		},
	})

	deps, parentMapForTest, err := r.ResolveAllRecursive(context.Background(), []parserlock.ActionRef{
		{Owner: "org", Repo: "fixtures", Path: "nested-composite", Ref: "main"},
	})
	if err != nil {
		t.Fatalf("ResolveAllRecursive returned error: %v", err)
	}

	// nested-composite + simple-composite collapse to one tarball entry
	// (org/fixtures@main); org/fixtures-b is the second. Two unique deps.
	if len(deps) != 2 {
		t.Fatalf("expected 2 unique deps, got %d: %+v", len(deps), deps)
	}
	foundB := false
	for _, d := range deps {
		if d.Key() == "org/fixtures-b@main" {
			foundB = true
		}
	}
	if !foundB {
		t.Fatalf("expected 2-levels-deep transitive org/fixtures-b@main to be discovered, got %+v", deps)
	}

	pm := parentMapForTest
	// The same-tarball edge must NOT create a self-parent on the tarball.
	for _, p := range pm["org/fixtures@main"] {
		if p == "org/fixtures@main" {
			t.Errorf("unexpected same-tarball self-parent edge on org/fixtures@main: %v", pm["org/fixtures@main"])
		}
	}
	// org/fixtures-b's parent is the shared tarball.
	if got := pm["org/fixtures-b@main"]; len(got) != 1 || got[0] != "org/fixtures@main" {
		t.Errorf("expected org/fixtures-b@main parent = [org/fixtures@main], got %v", got)
	}
}

// TestResolveAllRecursiveCrossRefTransitive verifies that when a composite at
// ref "updated" references a sibling subpath at a DIFFERENT ref "main", the
// BFS discovers the full transitive closure through the second composite.
// This mirrors nodeselector/actions-test-fixtures where:
//
//	nested-composite@updated → simple-composite@main → (simple-node@main + fixtures-b/simple-echo@main)
func TestResolveAllRecursiveCrossRefTransitive(t *testing.T) {
	r := seedCache(&Resolver{
		MaxRecursionDepth: DefaultMaxRecursionDepth,
	}, map[ghapi.ActionRef]resolvedEntry{
		ghapi.ForActionRef("org", "fixtures", "nested-composite", "updated"): {
			dep:       dep.Dependency{NWO: "org/fixtures", Path: "nested-composite", Ref: "updated", SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
			actionYML: "name: Nested\nruns:\n  using: composite\n  steps:\n    - uses: org/fixtures/simple-composite@main\n",
		},
		ghapi.ForActionRef("org", "fixtures", "simple-composite", "main"): {
			dep:       dep.Dependency{NWO: "org/fixtures", Path: "simple-composite", Ref: "main", SHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
			actionYML: "name: Simple\nruns:\n  using: composite\n  steps:\n    - uses: org/fixtures/simple-node@main\n    - uses: org/fixtures-b/simple-echo@main\n",
		},
		ghapi.ForActionRef("org", "fixtures", "simple-node", "main"): {
			dep:       dep.Dependency{NWO: "org/fixtures", Path: "simple-node", Ref: "main", SHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
			actionYML: "name: Node\nruns:\n  using: node20\n",
		},
		ghapi.ForActionRef("org", "fixtures-b", "simple-echo", "main"): {
			dep:       dep.Dependency{NWO: "org/fixtures-b", Path: "simple-echo", Ref: "main", SHA: "cccccccccccccccccccccccccccccccccccccccc"},
			actionYML: "name: Echo\nruns:\n  using: node20\n",
		},
	})

	deps, pm, err := r.ResolveAllRecursive(context.Background(), []parserlock.ActionRef{
		{Owner: "org", Repo: "fixtures", Path: "nested-composite", Ref: "updated"},
	})
	if err != nil {
		t.Fatalf("ResolveAllRecursive error: %v", err)
	}

	keys := map[string]bool{}
	for _, d := range deps {
		keys[d.Key()] = true
	}

	// We expect 3 unique deps (by Key = NWO@Ref):
	//   org/fixtures@updated  (from nested-composite)
	//   org/fixtures@main     (from simple-composite + simple-node, same tarball)
	//   org/fixtures-b@main   (from simple-echo, cross-repo transitive)
	want := []string{"org/fixtures@updated", "org/fixtures@main", "org/fixtures-b@main"}
	for _, w := range want {
		if !keys[w] {
			t.Errorf("missing expected dep %s; got keys: %v", w, keys)
		}
	}
	if len(deps) != len(want) {
		t.Errorf("expected %d unique deps, got %d: %+v", len(want), len(deps), deps)
	}

	// org/fixtures@main's parent is org/fixtures@updated
	if got := pm["org/fixtures@main"]; len(got) != 1 || got[0] != "org/fixtures@updated" {
		t.Errorf("expected org/fixtures@main parent = [org/fixtures@updated], got %v", got)
	}
	// org/fixtures-b@main's parent is org/fixtures@main
	if got := pm["org/fixtures-b@main"]; len(got) != 1 || got[0] != "org/fixtures@main" {
		t.Errorf("expected org/fixtures-b@main parent = [org/fixtures@main], got %v", got)
	}
}

// TestResolveAllRecursiveTerminatesOnCycle verifies that a mutual A→B→A cycle
// is handled gracefully: the BFS terminates via the seen set, both nodes are
// resolved, and the parentMap reflects the edges without infinite recursion.
func TestResolveAllRecursiveTerminatesOnCycle(t *testing.T) {
	r := seedCache(&Resolver{
		MaxRecursionDepth: DefaultMaxRecursionDepth,
	}, map[ghapi.ActionRef]resolvedEntry{
		ghapi.ForActionRef("owner", "a", "", "v1"): {
			dep:       dep.Dependency{NWO: "owner/a", Ref: "v1", SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Branch: "main", HashAlgo: "sha1"},
			actionYML: "name: A\nruns:\n  using: composite\n  steps:\n    - uses: owner/b@v1\n",
		},
		ghapi.ForActionRef("owner", "b", "", "v1"): {
			dep:       dep.Dependency{NWO: "owner/b", Ref: "v1", SHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Branch: "main", HashAlgo: "sha1"},
			actionYML: "name: B\nruns:\n  using: composite\n  steps:\n    - uses: owner/a@v1\n",
		},
	})

	deps, parentMapForTest, err := r.ResolveAllRecursive(context.Background(), []parserlock.ActionRef{{Owner: "owner", Repo: "a", Ref: "v1"}})
	if err != nil {
		t.Fatalf("ResolveAllRecursive returned error: %v", err)
	}
	if len(deps) != 2 {
		t.Fatalf("expected 2 deps (cycle terminates without duplication), got %d: %+v", len(deps), deps)
	}

	pm := parentMapForTest
	// A is parent of B (A uses B).
	if got := pm["owner/b@v1"]; len(got) != 1 || got[0] != "owner/a@v1" {
		t.Errorf("expected owner/b@v1 parent = [owner/a@v1], got %v", got)
	}
	// B→A edge: B uses A, but A was already resolved at depth 0. The
	// parentMap edge is recorded before the seen-filter discards the
	// re-enqueue, so the back-edge exists. This is safe because Save()'s
	// GC walk uses its own visited set to handle cycles.
	if got := pm["owner/a@v1"]; len(got) != 1 || got[0] != "owner/b@v1" {
		t.Errorf("expected owner/a@v1 parent = [owner/b@v1] (back-edge), got %v", got)
	}
}

func TestResolveAllRecursiveCompositeLocalPathError(t *testing.T) {
	r := seedCache(&Resolver{
		MaxRecursionDepth: DefaultMaxRecursionDepth,
	}, map[ghapi.ActionRef]resolvedEntry{
		ghapi.ForActionRef("owner", "composite", "", "v1"): {
			dep:       dep.Dependency{NWO: "owner/composite", Ref: "v1", SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
			actionYML: "name: Composite\nruns:\n  using: composite\n  steps:\n    - uses: ./helper\n    - uses: actions/checkout@v6\n",
		},
		ghapi.ForActionRef("actions", "checkout", "", "v6"): {
			dep:       dep.Dependency{NWO: "actions/checkout", Ref: "v6", SHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
			actionYML: "name: Checkout\nruns:\n  using: node20\n",
		},
	})

	deps, _, err := r.ResolveAllRecursive(context.Background(), []parserlock.ActionRef{
		{Owner: "owner", Repo: "composite", Ref: "v1"},
	})
	if err == nil {
		t.Fatal("expected CompositeLocalPathError, got nil")
	}
	if !IsCompositeLocalPath(err) {
		t.Fatalf("expected CompositeLocalPathError, got: %v", err)
	}
	if !strings.Contains(err.Error(), "./helper") {
		t.Errorf("error should mention the local path, got: %v", err)
	}
	// Partial results should still be returned (checkout resolved).
	if len(deps) < 2 {
		t.Errorf("expected partial results with at least 2 deps, got %d", len(deps))
	}
}

// TestResolveAllRecursiveSelfRepoNestedInComposite verifies that a `$/…`
// self-reference inside a fetched composite is a same-tarball edge: it records
// no new pin, but the BFS still descends into the sibling sub-action so its
// cross-repo transitive deps are discovered. Mirrors the runner's
// PrepareActions_SelfRepository_ResolvesNestedInComposite.
func TestResolveAllRecursiveSelfRepoNestedInComposite(t *testing.T) {
	const tarballSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	r := seedCache(&Resolver{
		MaxRecursionDepth: DefaultMaxRecursionDepth,
	}, map[ghapi.ActionRef]resolvedEntry{
		ghapi.ForActionRef("org", "fixtures", "parent", "main"): {
			dep:       dep.Dependency{NWO: "org/fixtures", Path: "parent", Ref: "main", SHA: tarballSHA},
			actionYML: "name: Parent\nruns:\n  using: composite\n  steps:\n    - uses: $/child\n",
		},
		ghapi.ForActionRef("org", "fixtures", "child", "main"): {
			dep:       dep.Dependency{NWO: "org/fixtures", Path: "child", Ref: "main", SHA: tarballSHA},
			actionYML: "name: Child\nruns:\n  using: composite\n  steps:\n    - uses: other/repo@v2\n",
		},
		ghapi.ForActionRef("other", "repo", "", "v2"): {
			dep:       dep.Dependency{NWO: "other/repo", Ref: "v2", SHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
			actionYML: "name: Other\nruns:\n  using: node20\n",
		},
	})

	deps, pm, err := r.ResolveAllRecursive(context.Background(), []parserlock.ActionRef{
		{Owner: "org", Repo: "fixtures", Path: "parent", Ref: "main"},
	})
	if err != nil {
		t.Fatalf("ResolveAllRecursive error: %v", err)
	}

	// parent + child collapse to org/fixtures@main; other/repo@v2 is the
	// cross-repo transitive discovered via the $/ sibling. Two unique deps.
	keys := map[string]bool{}
	for _, d := range deps {
		keys[d.Key()] = true
	}
	if !keys["other/repo@v2"] {
		t.Fatalf("expected $/ sibling's transitive other/repo@v2 to be discovered, got %v", keys)
	}
	if len(deps) != 2 {
		t.Fatalf("expected 2 unique deps (tarball + transitive), got %d: %+v", len(deps), deps)
	}
	// The same-tarball $/ edge must not record a self-parent.
	for _, p := range pm["org/fixtures@main"] {
		if p == "org/fixtures@main" {
			t.Errorf("unexpected same-tarball self-parent on org/fixtures@main: %v", pm["org/fixtures@main"])
		}
	}
	// other/repo@v2's parent is the shared tarball.
	if got := pm["other/repo@v2"]; len(got) != 1 || got[0] != "org/fixtures@main" {
		t.Errorf("expected other/repo@v2 parent = [org/fixtures@main], got %v", got)
	}
}

// TestResolveAllRecursiveSelfRepoResolvesToParentRepo verifies that a `$/…`
// inside a CROSS-repo composite resolves against that composite's own repo and
// ref — not the top-level workflow-run repo. Mirrors the runner's
// _CrossRepoCompositeResolvesToParentRepo.
func TestResolveAllRecursiveSelfRepoResolvesToParentRepo(t *testing.T) {
	const tarballSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	r := seedCache(&Resolver{
		MaxRecursionDepth: DefaultMaxRecursionDepth,
	}, map[ghapi.ActionRef]resolvedEntry{
		ghapi.ForActionRef("external", "foo", "", "v1"): {
			dep:       dep.Dependency{NWO: "external/foo", Ref: "v1", SHA: tarballSHA},
			actionYML: "name: Foo\nruns:\n  using: composite\n  steps:\n    - uses: $/lib/bar\n",
		},
		// Reachable only if $/lib/bar resolved under external/foo@v1.
		ghapi.ForActionRef("external", "foo", "lib/bar", "v1"): {
			dep:       dep.Dependency{NWO: "external/foo", Path: "lib/bar", Ref: "v1", SHA: tarballSHA},
			actionYML: "name: Bar\nruns:\n  using: composite\n  steps:\n    - uses: someother/dep@v3\n",
		},
		ghapi.ForActionRef("someother", "dep", "", "v3"): {
			dep:       dep.Dependency{NWO: "someother/dep", Ref: "v3", SHA: "cccccccccccccccccccccccccccccccccccccccc"},
			actionYML: "name: Dep\nruns:\n  using: node20\n",
		},
	})

	deps, _, err := r.ResolveAllRecursive(context.Background(), []parserlock.ActionRef{
		{Owner: "external", Repo: "foo", Ref: "v1"},
	})
	if err != nil {
		t.Fatalf("ResolveAllRecursive error: %v", err)
	}

	keys := map[string]bool{}
	for _, d := range deps {
		keys[d.Key()] = true
	}
	// If $/lib/bar had resolved against the wrong repo, its cross-repo dep
	// would never be found.
	if !keys["someother/dep@v3"] {
		t.Fatalf("expected $/lib/bar to resolve under external/foo@v1 and discover someother/dep@v3, got %v", keys)
	}
	if len(deps) != 2 {
		t.Fatalf("expected 2 unique deps (external/foo tarball + someother/dep), got %d: %+v", len(deps), deps)
	}
}

// TestResolveAllRecursiveSelfRepoMultiLevelChain verifies a `$/a → $/b → $/c`
// same-tarball chain descends all the way to c's cross-repo dep. Mirrors the
// runner's _MultiLevelChain.
func TestResolveAllRecursiveSelfRepoMultiLevelChain(t *testing.T) {
	const tarballSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	mk := func(path, use string) resolvedEntry {
		return resolvedEntry{
			dep:       dep.Dependency{NWO: "org/x", Path: path, Ref: "main", SHA: tarballSHA},
			actionYML: "name: " + path + "\nruns:\n  using: composite\n  steps:\n    - uses: " + use + "\n",
		}
	}
	r := seedCache(&Resolver{
		MaxRecursionDepth: DefaultMaxRecursionDepth,
	}, map[ghapi.ActionRef]resolvedEntry{
		ghapi.ForActionRef("org", "x", "root", "main"): mk("root", "$/a"),
		ghapi.ForActionRef("org", "x", "a", "main"):    mk("a", "$/b"),
		ghapi.ForActionRef("org", "x", "b", "main"):    mk("b", "$/c"),
		ghapi.ForActionRef("org", "x", "c", "main"):    mk("c", "leaf/dep@v9"),
		ghapi.ForActionRef("leaf", "dep", "", "v9"): {
			dep:       dep.Dependency{NWO: "leaf/dep", Ref: "v9", SHA: "dddddddddddddddddddddddddddddddddddddddd"},
			actionYML: "name: Leaf\nruns:\n  using: node20\n",
		},
	})

	deps, _, err := r.ResolveAllRecursive(context.Background(), []parserlock.ActionRef{
		{Owner: "org", Repo: "x", Path: "root", Ref: "main"},
	})
	if err != nil {
		t.Fatalf("ResolveAllRecursive error: %v", err)
	}

	keys := map[string]bool{}
	for _, d := range deps {
		keys[d.Key()] = true
	}
	// All $/ hops collapse to org/x@main; leaf/dep@v9 is 3 levels deep.
	if !keys["leaf/dep@v9"] {
		t.Fatalf("expected 3-level $/ chain to reach leaf/dep@v9, got %v", keys)
	}
	if len(deps) != 2 {
		t.Fatalf("expected 2 unique deps (org/x tarball + leaf/dep), got %d: %+v", len(deps), deps)
	}
}

func TestNewAndLatestRef(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	reg.Register(
		httpmock.REST(http.MethodGet, "repos/actions/checkout/tags"),
		httpmock.JSONResponse([]map[string]any{
			{"name": "v4", "commit": map[string]string{"sha": "aaa"}},
			{"name": "v5", "commit": map[string]string{"sha": "bbb"}},
			{"name": "v6", "commit": map[string]string{"sha": "ccc"}},
		}),
	)

	r, err := New("github.com", pinpool.New(2, nil), WithTransport(reg))
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	if host := r.Hostname(); host != "github.com" {
		t.Fatalf("expected hostname github.com, got %q", host)
	}

	ref, err := r.LatestRef(context.Background(), "actions", "checkout")
	if err != nil {
		t.Fatalf("LatestRef returned error: %v", err)
	}
	if ref != "v6" {
		t.Fatalf("expected latest ref v6, got %q", ref)
	}

	// Second call should hit the cache instead of requiring another stub.
	ref, err = r.LatestRef(context.Background(), "actions", "checkout")
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
		httpmock.GraphQLForRepo("owner", "composite"),
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
		httpmock.GraphQLForRepo("actions", "checkout"),
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

	r, err := New("github.com", pinpool.New(2, nil), WithTransport(reg))
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	deps, _, err := r.ResolveAllRecursive(context.Background(), []parserlock.ActionRef{{Owner: "owner", Repo: "composite", Ref: "v1"}})
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

// TestResolveAllRecursivePartialFailure verifies that when one ref in a batch
// fails (e.g. repo not found), the successful refs are still returned alongside
// the error. This is the resolver-level contract that enables the cascade fix:
// planWorkflow can use partial results instead of marking everything unresolved.
func TestResolveAllRecursivePartialFailure(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	// Both refs fold into one batched query: a0=good/action resolves,
	// a1=bad/private comes back null (repo not found). The single stub
	// matches the batch (its vars contain good/action).
	reg.Register(
		httpmock.GraphQLForRepo("good", "action"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": map[string]any{
					"nameWithOwner": "good/action",
					"object": map[string]any{
						"oid":  "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
						"file": map[string]any{"object": map[string]any{"text": "name: Good\nruns:\n  using: node20\n"}},
					},
				},
				"a1": nil,
			},
		}),
	)

	r, err := New("github.com", pinpool.New(2, nil), WithTransport(reg))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	deps, _, resolveErr := r.ResolveAllRecursive(context.Background(), []parserlock.ActionRef{
		{Owner: "good", Repo: "action", Ref: "v1"},
		{Owner: "bad", Repo: "private", Ref: "main"},
	})

	// Must return an error (bad/private failed).
	if resolveErr == nil {
		t.Fatal("expected error for inaccessible repo, got nil")
	}
	if !strings.Contains(resolveErr.Error(), "not found") {
		t.Fatalf("expected 'not found' in error, got: %v", resolveErr)
	}

	// Must return partial results (good/action resolved).
	if len(deps) != 1 {
		t.Fatalf("expected 1 partial dep (good/action), got %d: %+v", len(deps), deps)
	}
	if deps[0].NWO != "good/action" {
		t.Fatalf("expected good/action dep, got %q", deps[0].NWO)
	}
	if deps[0].SHA != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("expected correct SHA, got %q", deps[0].SHA)
	}
}

// TestResolveAllRecursivePartialFailureCachedGood verifies partial failure
// when the successful ref comes from the resolver cache (no HTTP needed)
// and only the failing ref hits the network.
func TestResolveAllRecursivePartialFailureCachedGood(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	// bad/private returns null — repo not found.
	reg.Register(
		httpmock.GraphQLForRepo("bad", "private"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": nil,
			},
		}),
	)

	r, err := New("github.com", pinpool.New(2, nil), WithTransport(reg))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Pre-seed cache with the good ref.
	seedCache(r, map[ghapi.ActionRef]resolvedEntry{
		ghapi.ForActionRef("good", "action", "", "v1"): {
			dep:       dep.Dependency{NWO: "good/action", Ref: "v1", SHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
			actionYML: "name: Good\nruns:\n  using: node20\n",
		},
	})

	deps, _, resolveErr := r.ResolveAllRecursive(context.Background(), []parserlock.ActionRef{
		{Owner: "good", Repo: "action", Ref: "v1"},
		{Owner: "bad", Repo: "private", Ref: "main"},
	})

	if resolveErr == nil {
		t.Fatal("expected error for inaccessible repo, got nil")
	}

	if len(deps) != 1 {
		t.Fatalf("expected 1 partial dep, got %d: %+v", len(deps), deps)
	}
	if deps[0].NWO != "good/action" {
		t.Fatalf("expected good/action, got %q", deps[0].NWO)
	}
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

// TestDiscoverContaining_BranchBeyondPageCap_NotImpostor is the discovery-path
// counterpart: a commit on a branch that sorts past the listing cap must be
// placed on that branch (fetched directly) rather than flagged as an impostor.
func TestDiscoverContaining_BranchBeyondPageCap_NotImpostor(t *testing.T) {
	sha := "aafc3630d7b9aafc3630d7b9aafc3630d7b9aafc"

	reg := &httpmock.Registry{}
	reg.Register(
		httpmock.REST("GET", `repos/vercel/next.js/git/ref/heads/canary`),
		httpmock.JSONResponse(gitRefHeadResponse("canary", sha)),
	)
	reg.Register(
		httpmock.REST("GET", `repos/vercel/next.js/tags`),
		httpmock.JSONResponse(httpmock.TagListResponse()),
	)

	r, err := New("github.com", pinpool.New(2, nil), WithTransport(reg))
	if err != nil {
		t.Fatal(err)
	}

	_, branch, err := r.DiscoverContainingDefault(context.Background(), "vercel", "next.js", sha, "canary", "canary")
	if err != nil {
		t.Fatalf("unexpected error (false impostor?): %v", err)
	}
	if branch != "canary" {
		t.Fatalf("expected branch=canary via direct fetch, got %q", branch)
	}
	reg.Verify(t)
}

// peelResponse builds a GraphQL response body for the tag-object peel
// query: typename is the __typename returned for object(oid:$oid), and
// peeledOID is the commit returned for object(expression:$oid^{commit}).
// Pass typename=="" to omit the head object entirely (simulating "OID not
// found"); pass peeledOID=="" to omit the peeled commit fragment.
func peelResponse(typename, peeledOID string) map[string]any {
	repo := map[string]any{}
	if typename != "" {
		repo["head"] = map[string]any{"__typename": typename}
	} else {
		repo["head"] = nil
	}
	if peeledOID != "" {
		repo["peeled"] = map[string]any{"oid": peeledOID}
	} else {
		repo["peeled"] = nil
	}
	return map[string]any{"data": map[string]any{"repository": repo}}
}

func TestPeelTagObjectAnnotatedTagOneRoundTrip(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	// One stub — one round trip. If the implementation falls back to a
	// recursive walk it will fail "no registered HTTP stubs matched".
	tagSHA := "1111111111111111111111111111111111111111"
	commitSHA := "2222222222222222222222222222222222222222"
	reg.Register(
		httpmock.GraphQLForRepo("owner", "repo"),
		httpmock.JSONResponse(peelResponse("Tag", commitSHA)),
	)

	r, err := New("github.com", pinpool.New(2, nil), WithTransport(reg))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	got, ok := r.PeelTagObject(context.Background(), "owner", "repo", tagSHA)
	if !ok {
		t.Fatalf("expected ok=true for annotated tag object")
	}
	if got != commitSHA {
		t.Fatalf("expected peeled commit %q, got %q", commitSHA, got)
	}
	if !r.IsKnownTagObject("owner", "repo", tagSHA) {
		t.Fatalf("expected cache to mark tag-object SHA as known")
	}
}

func TestPeelTagObjectDeepChainStillOneRoundTrip(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	// A tag-of-tag-of-tag chain seven levels deep — well past the prior
	// REST walk's hardcoded depth cap of 5. The GraphQL `^{commit}` peel
	// happens server-side, so the client still sees exactly one stub.
	tagSHA := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	commitSHA := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	reg.Register(
		httpmock.GraphQLForRepo("owner", "deep"),
		httpmock.JSONResponse(peelResponse("Tag", commitSHA)),
	)

	r, err := New("github.com", pinpool.New(2, nil), WithTransport(reg))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	got, ok := r.PeelTagObject(context.Background(), "owner", "deep", tagSHA)
	if !ok || got != commitSHA {
		t.Fatalf("expected commit %q ok=true, got %q ok=%v", commitSHA, got, ok)
	}
}

func TestPeelTagObjectPlainCommitNegativeCached(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	commitSHA := "3333333333333333333333333333333333333333"
	// Plain-commit SHA: __typename is "Commit", not "Tag". One stub only —
	// the second call must hit the negative cache instead of re-querying.
	reg.Register(
		httpmock.GraphQLForRepo("owner", "plain"),
		httpmock.JSONResponse(peelResponse("Commit", commitSHA)),
	)

	r, err := New("github.com", pinpool.New(2, nil), WithTransport(reg))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, ok := r.PeelTagObject(context.Background(), "owner", "plain", commitSHA); ok {
		t.Fatalf("expected ok=false for plain commit SHA")
	}
	if r.IsKnownTagObject("owner", "plain", commitSHA) {
		t.Fatalf("plain commit must not be marked as tag object")
	}
	// Second call: no new stub — proves the negative result is cached.
	if _, ok := r.PeelTagObject(context.Background(), "owner", "plain", commitSHA); ok {
		t.Fatalf("cached negative result flipped to ok=true")
	}
}

func TestPeelTagObjectUnknownSHANotCached(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	unknownSHA := "4444444444444444444444444444444444444444"
	// OID not present in the repo: GraphQL returns repository.head == null.
	// We must NOT cache — the SHA may appear after a fetch or permission
	// grant, so a follow-up call has to re-query.
	reg.Register(
		httpmock.GraphQLForRepo("owner", "unknown"),
		httpmock.JSONResponse(peelResponse("", "")),
	)
	reg.Register(
		httpmock.GraphQLForRepo("owner", "unknown"),
		httpmock.JSONResponse(peelResponse("Tag", "5555555555555555555555555555555555555555")),
	)

	r, err := New("github.com", pinpool.New(2, nil), WithTransport(reg))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, ok := r.PeelTagObject(context.Background(), "owner", "unknown", unknownSHA); ok {
		t.Fatalf("expected ok=false when OID is unknown")
	}
	got, ok := r.PeelTagObject(context.Background(), "owner", "unknown", unknownSHA)
	if !ok || got != "5555555555555555555555555555555555555555" {
		t.Fatalf("expected retry to succeed with peeled commit, got %q ok=%v", got, ok)
	}
}

func TestPeelTagObjectTransientErrorNotCached(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	sha := "6666666666666666666666666666666666666666"
	commitSHA := "7777777777777777777777777777777777777777"
	// First call: 500. Must not cache — second call retries and succeeds.
	reg.Register(
		httpmock.GraphQLForRepo("owner", "flaky"),
		httpmock.StatusResponse(500),
	)
	reg.Register(
		httpmock.GraphQLForRepo("owner", "flaky"),
		httpmock.JSONResponse(peelResponse("Tag", commitSHA)),
	)

	r, err := New("github.com", pinpool.New(2, nil), WithTransport(reg))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, ok := r.PeelTagObject(context.Background(), "owner", "flaky", sha); ok {
		t.Fatalf("expected ok=false on transient error")
	}
	got, ok := r.PeelTagObject(context.Background(), "owner", "flaky", sha)
	if !ok || got != commitSHA {
		t.Fatalf("expected retry to succeed, got %q ok=%v", got, ok)
	}
}

func TestNew_Options(t *testing.T) {
	reg := &httpmock.Registry{}
	pool := pinpool.New(1, nil)

	fixed := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	nowFn := func() time.Time { return fixed }
	sleepCalled := false
	sleepFn := func(_ context.Context, _ time.Duration) { sleepCalled = true }

	r, err := New("test.com", pool,
		WithTransport(reg),
		WithNowFn(nowFn),
		WithSleepFn(sleepFn),
	)
	if err != nil {
		t.Fatal(err)
	}

	if got := r.nowFn(); !got.Equal(fixed) {
		t.Fatalf("NowFn not applied: got %v", got)
	}

	r.sleepFn(context.Background(), 0)
	if !sleepCalled {
		t.Fatal("SleepFn not applied")
	}
}

func TestNew_NilOptionsSafe(t *testing.T) {
	reg := &httpmock.Registry{}
	pool := pinpool.New(1, nil)

	// nil functions should not override defaults.
	r, err := New("test.com", pool,
		WithTransport(reg),
		WithNowFn(nil),
		WithSleepFn(nil),
	)
	if err != nil {
		t.Fatal(err)
	}

	if r.nowFn == nil {
		t.Fatal("nowFn should not be nil after WithNowFn(nil)")
	}
	if r.sleepFn == nil {
		t.Fatal("sleepFn should not be nil after WithSleepFn(nil)")
	}
}

// TestResolveAllRecursivePartialFailureBFSContinues verifies that when one ref
// in a batch fails (e.g. SSO/403), the BFS still discovers transitive deps
// from composite actions that DID resolve. Before the fix, the early return on
// error skipped the BFS loop entirely.
func TestResolveAllRecursivePartialFailureBFSContinues(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	// Batch contains two refs: a0=my/composite resolves as a composite with
	// a transitive dep on other/action@main. a1=sso/blocked returns null.
	reg.Register(
		httpmock.GraphQLForRepo("my", "composite"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": map[string]any{
					"nameWithOwner": "my/composite",
					"object": map[string]any{
						"oid": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
						"file": map[string]any{"object": map[string]any{
							"text": "name: MyComposite\nruns:\n  using: composite\n  steps:\n    - uses: other/action@main\n",
						}},
					},
				},
				"a1": nil,
			},
		}),
	)

	// Transitive dep: other/action@main resolves as a node action (leaf).
	reg.Register(
		httpmock.GraphQLForRepo("other", "action"),
		httpmock.JSONResponse(map[string]any{
			"data": map[string]any{
				"a0": map[string]any{
					"nameWithOwner": "other/action",
					"object": map[string]any{
						"oid":  "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
						"file": map[string]any{"object": map[string]any{"text": "name: OtherAction\nruns:\n  using: node20\n"}},
					},
				},
			},
		}),
	)

	r, err := New("github.com", pinpool.New(2, nil), WithTransport(reg))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	deps, pm, resolveErr := r.ResolveAllRecursive(context.Background(), []parserlock.ActionRef{
		{Owner: "my", Repo: "composite", Ref: "v1"},
		{Owner: "sso", Repo: "blocked", Ref: "v4"},
	})

	// Must return an error (sso/blocked failed).
	if resolveErr == nil {
		t.Fatal("expected error for inaccessible repo, got nil")
	}

	// Must return BOTH the composite and its transitive dep.
	if len(deps) != 2 {
		t.Fatalf("expected 2 deps (composite + transitive), got %d: %+v", len(deps), deps)
	}

	nwos := map[string]bool{}
	for _, d := range deps {
		nwos[d.NWO] = true
	}
	if !nwos["my/composite"] {
		t.Error("missing my/composite in deps")
	}
	if !nwos["other/action"] {
		t.Error("missing transitive dep other/action in deps")
	}

	// ParentMap should record the transitive edge.
	parents, ok := pm["other/action@main"]
	if !ok || len(parents) == 0 {
		t.Fatalf("expected parentMap entry for other/action@main, got %v", pm)
	}
	if parents[0] != "my/composite@v1" {
		t.Fatalf("expected parent my/composite@v1, got %q", parents[0])
	}
}
