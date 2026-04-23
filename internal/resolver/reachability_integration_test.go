//go:build integration

// Integration tests for reachability checks using the GitHub Compare API.
// Requires: network access, GH_TOKEN or gh CLI auth.
// Fixtures:
//   - nodeselector/actions-test-fixtures: tag v1 on HEAD (ea53476), orphan-poison branch (614a37a)
//   - choam-io/actions-test-fixtures-fork: fork with attacker-payload branch (7b403c9)
//
// Run: go test -tags integration -run TestIntegration ./internal/resolver/

package resolver

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	fixtureOwner = "nodeselector"
	fixtureRepo  = "actions-test-fixtures"

	// HEAD of main, also where tag v1 points
	headSHA = "ea53476fdc172d8552df5af9658a45a367e4f41d"
	// Parent of HEAD — in v1's lineage but not at HEAD (tag-drift scenario)
	parentSHA = "38b3412adcb7afb4a061c519513e45cbaf4a1cec"
	// Root commit of main (oldest ancestor)
	rootSHA = "5f13f2a16a43112afcd6e1bcc29c418176894d53"
	// Orphan commit on orphan-poison branch (no common ancestor with main)
	orphanSHA = "614a37a63d1a75476792a8781b55983a9d9bcb80"
	// A SHA that doesn't exist anywhere
	fakeSHA = "0000000000000000000000000000000000000000"
	// Commit on choam-io/actions-test-fixtures-fork attacker-payload branch
	// This SHA exists in the fork network but NOT in the upstream repo's lineage
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

	result := r.CheckReachability(fixtureOwner, fixtureRepo, headSHA, "v1")
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

// TestIntegration_Unreachable_ForkNetworkInjection is the KEY test proving
// the Compare API merge-base identity check detects fork-network injection.
//
// The Compare API operates on the fork-network-shared object store, so the
// fork commit IS visible. However, the merge_base_commit for a fork commit
// will NOT be the fork SHA itself — it will be the actual common ancestor in
// the upstream history. This mismatch (merge_base != pinnedSHA) is the signal
// that detects the imposter commit.
func TestIntegration_Unreachable_ForkNetworkInjection(t *testing.T) {
	skipWithoutAuth(t)
	r := newLiveResolver(t)

	result := r.CheckReachability(fixtureOwner, fixtureRepo, forkAttackerSHA, "v1")
	assert.Equal(t, Unreachable, result.Status,
		"fork-network SHA should NOT be reachable via merge-base identity check: %+v", result)
}

// TestIntegration_SHAAsRef_ReturnsUnknown verifies that when the ref is itself
// a raw SHA (the anti-pattern), we return Unknown with guidance to pin to a tag.
func TestIntegration_SHAAsRef_ReturnsUnknown(t *testing.T) {
	skipWithoutAuth(t)
	r := newLiveResolver(t)

	result := r.CheckReachability(fixtureOwner, fixtureRepo, headSHA, headSHA)
	assert.Equal(t, ReachabilityUnknown, result.Status,
		"SHA-as-ref should return Unknown: %+v", result)
	assert.Contains(t, result.Detail, "pin to a tag")
}

// TestIntegration_CacheConsistency verifies that repeated calls return
// the same result and hit the cache on the second call.
func TestIntegration_CacheConsistency(t *testing.T) {
	skipWithoutAuth(t)
	r := newLiveResolver(t)

	r1 := r.CheckReachability(fixtureOwner, fixtureRepo, headSHA, "v1")
	r2 := r.CheckReachability(fixtureOwner, fixtureRepo, headSHA, "v1")
	assert.Equal(t, r1.Status, r2.Status)
	assert.Equal(t, "cached", r2.Detail, "second call should come from cache")
}
