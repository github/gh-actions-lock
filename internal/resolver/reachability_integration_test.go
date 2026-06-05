//go:build integration

// Integration tests for reachability checks using the GitHub Compare API.
// Requires: network access, GH_TOKEN or gh CLI auth.
// Fixtures:
//   - nodeselector/actions-test-fixtures: tag v1 on HEAD (ea53476), orphan-poison branch (614a37a),
//     annotated tag `annotated-v1` whose tag object peels to ea53476
//   - choam-io/actions-test-fixtures-fork: fork with attacker-payload branch (7b403c9)
//
// Run: go test -tags integration -run TestIntegration ./internal/resolver/

package resolver

import (
	"os"
	"testing"

	"github.com/github/gh-actions-pin/internal/lockfile"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	fixtureOwner = "nodeselector"
	fixtureRepo  = "actions-test-fixtures"

	// Where tag v1 points (was HEAD of main at time of fixture creation)
	v1SHA = "ea53476fdc172d8552df5af9658a45a367e4f41d"
	// Parent of v1 — in v1's lineage but not at v1 HEAD (tag-drift scenario)
	parentSHA = "38b3412adcb7afb4a061c519513e45cbaf4a1cec"
	// Root commit of main (oldest ancestor)
	rootSHA = "5f13f2a16a43112afcd6e1bcc29c418176894d53"
	// Orphan commit on orphan-poison branch (no common ancestor with main)
	orphanSHA = "614a37a63d1a75476792a8781b55983a9d9bcb80"
	// A SHA that doesn't exist anywhere
	fakeSHA = "0000000000000000000000000000000000000000"
	// Commit on choam-io/actions-test-fixtures-fork attacker-payload branch.
	// This SHA exists in the fork network but NOT in the upstream repo's branches.
	// Tag "tampered" in the upstream repo has been moved to point at this commit,
	// simulating a fork-network injection attack with preserved lineage.
	forkAttackerSHA = "7b403c9ec14bd3ae0bbf793c2bee8815a7ac920a"
)

func skipWithoutAuth(t *testing.T) {
	t.Helper()
	if os.Getenv("GH_TOKEN") == "" && os.Getenv("GITHUB_TOKEN") == "" {
		if _, err := os.Stat(os.ExpandEnv("$HOME/.config/gh/hosts.yml")); err != nil {
			t.Skip("Skipping integration test: no GH_TOKEN or gh auth configured")
		}
	}
}

func newLiveResolver(t *testing.T) *Resolver {
	t.Helper()
	r, err := New("github.com")
	require.NoError(t, err)
	return r
}

// TestIntegration_Reachable_HeadSHA verifies that the HEAD commit (where v1
// points) is reported as reachable via the Compare API merge-base identity check.
func TestIntegration_Reachable_HeadSHA(t *testing.T) {
	skipWithoutAuth(t)
	r := newLiveResolver(t)

	result := r.CheckReachability(fixtureOwner, fixtureRepo, v1SHA, "v1")
	assert.Equal(t, Reachable, result.Status, "HEAD SHA should be reachable from v1: %+v", result)
}

// TestIntegration_Reachable_Ancestor verifies that the root commit of main
// is reachable from v1 (it's an ancestor).
func TestIntegration_Reachable_Ancestor(t *testing.T) {
	skipWithoutAuth(t)
	r := newLiveResolver(t)

	result := r.CheckReachability(fixtureOwner, fixtureRepo, rootSHA, "v1")
	assert.Equal(t, Reachable, result.Status, "root commit should be ancestor of v1: %+v", result)
}

// TestIntegration_Reachable_NotAtHead_ButInLineage simulates tag drift: the
// pinned SHA was once at the tag's HEAD but the tag has since moved forward.
// The pinned SHA (parent of current HEAD) should still be reachable.
func TestIntegration_Reachable_NotAtHead_ButInLineage(t *testing.T) {
	skipWithoutAuth(t)
	r := newLiveResolver(t)

	result := r.CheckReachability(fixtureOwner, fixtureRepo, parentSHA, "v1")
	assert.Equal(t, Reachable, result.Status,
		"commit behind HEAD should still be reachable from v1 (tag drift): %+v", result)
}

// TestIntegration_Unreachable_OrphanCommit verifies that a commit on an orphan
// branch (no common ancestor with main) is detected as unreachable from v1.
func TestIntegration_Unreachable_OrphanCommit(t *testing.T) {
	skipWithoutAuth(t)
	r := newLiveResolver(t)

	result := r.CheckReachability(fixtureOwner, fixtureRepo, orphanSHA, "v1")
	assert.Equal(t, Unreachable, result.Status, "orphan commit should not be reachable from v1: %+v", result)
}

// TestIntegration_Unreachable_NonexistentSHA verifies that a completely
// fabricated SHA is detected as unreachable.
func TestIntegration_Unreachable_NonexistentSHA(t *testing.T) {
	skipWithoutAuth(t)
	r := newLiveResolver(t)

	result := r.CheckReachability(fixtureOwner, fixtureRepo, fakeSHA, "v1")
	assert.Equal(t, Unreachable, result.Status, "fake SHA should be unreachable: %+v", result)
}

// TestIntegration_Unreachable_ForkNetworkInjection tests that a fork commit
// used as a pinned SHA against a clean tag is detected as unreachable.
// branch_commits returns no branches for fork-network commits in the upstream repo.
func TestIntegration_Unreachable_ForkNetworkInjection(t *testing.T) {
	skipWithoutAuth(t)
	r := newLiveResolver(t)

	result := r.CheckReachability(fixtureOwner, fixtureRepo, forkAttackerSHA, "v1")
	assert.Equal(t, Unreachable, result.Status,
		"fork-network SHA should NOT be reachable via branch_commits check: %+v", result)
}

// TestIntegration_Unreachable_ForkNetworkInjection_PreservedLineage is the
// critical test for tag manipulation attacks with preserved lineage.
//
// Scenario: tag "tampered" has been moved to point at forkAttackerSHA (7b403c9),
// a commit in choam-io/actions-test-fixtures-fork. This fork commit has a real
// upstream parent (fbe0421), so it PRESERVES LINEAGE — the old pinned SHA
// (v1SHA / ea53476) IS an ancestor of the fork commit.
//
// The branch_commits endpoint catches this because fork-network commits have
// no branches in the upstream repo, regardless of their ancestry.
func TestIntegration_Unreachable_ForkNetworkInjection_PreservedLineage(t *testing.T) {
	skipWithoutAuth(t)
	r := newLiveResolver(t)

	// v1SHA (ea53476) was the original pinned SHA.
	// Tag "tampered" now points to forkAttackerSHA (7b403c9) which descends
	// from v1SHA through the fork network. Without the containment check,
	// this would incorrectly pass as Reachable.
	result := r.CheckReachability(fixtureOwner, fixtureRepo, v1SHA, "tampered")
	assert.Equal(t, Unreachable, result.Status,
		"fork commit via tag manipulation should be detected even with preserved lineage: %+v", result)
	assert.Contains(t, result.Detail, "fork-network",
		"detail should mention fork-network injection: %s", result.Detail)
}

// TestIntegration_SHAAsRef_ReturnsReachableWithWarning verifies that when the
// ref is itself a raw SHA (the anti-pattern), we return Reachable with guidance
// to pin to a tag instead, since origin can't be verified at job runtime.
func TestIntegration_SHAAsRef_ReturnsReachableWithWarning(t *testing.T) {
	skipWithoutAuth(t)
	r := newLiveResolver(t)

	result := r.CheckReachability(fixtureOwner, fixtureRepo, v1SHA, v1SHA)
	assert.Equal(t, Reachable, result.Status,
		"SHA-as-ref with valid commit should return Reachable: %+v", result)
	assert.Contains(t, result.Detail, "bare SHA",
		"detail should warn about bare SHA pinning: %s", result.Detail)
}

// TestIntegration_CacheConsistency verifies that repeated calls return
// the same result and hit the cache on the second call.
func TestIntegration_CacheConsistency(t *testing.T) {
	skipWithoutAuth(t)
	r := newLiveResolver(t)

	r1 := r.CheckReachability(fixtureOwner, fixtureRepo, v1SHA, "v1")
	r2 := r.CheckReachability(fixtureOwner, fixtureRepo, v1SHA, "v1")
	assert.Equal(t, r1.Status, r2.Status)
	assert.Equal(t, "cached", r2.Detail, "second call should come from cache")
}

// TestIntegration_AnnotatedTagPeeling verifies that an annotated tag resolves
// to its underlying commit SHA, not the tag-object SHA. The fixture repo has
// annotated tag `annotated-v1` whose tag object peels to v1SHA (ea53476).
//
// Without the `^{commit}` peel in buildResolveWithFileQuery, GraphQL returns
// a Tag object for `object(expression: "annotated-v1")` and the
// `... on Commit { oid }` fragment misses, leaving SHA empty.
func TestIntegration_AnnotatedTagPeeling(t *testing.T) {
	skipWithoutAuth(t)
	r := newLiveResolver(t)

	deps, parentMapForTest, err := r.ResolveAllRecursive([]lockfile.ActionRef{
		{Owner: fixtureOwner, Repo: fixtureRepo, Ref: "annotated-v1"},
	})
	require.NoError(t, err, "annotated tag must resolve through the peel")
	require.NotEmpty(t, deps)

	var found *lockfile.Dependency
	for i := range deps {
		if deps[i].NWO == fixtureOwner+"/"+fixtureRepo && deps[i].Ref == "annotated-v1" {
			found = &deps[i]
			break
		}
	}
	require.NotNil(t, found, "expected annotated-v1 dep in results: %+v", deps)
	assert.Equal(t, v1SHA, found.SHA,
		"annotated tag must peel to the underlying commit SHA, not the tag-object SHA")
}
