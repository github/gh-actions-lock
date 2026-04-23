//go:build integration

// Integration tests for reachability checks using git bare clones.
// Requires: git CLI, network access, GH_TOKEN or gh CLI auth.
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
	// Root commit of main (oldest ancestor)
	rootSHA = "5f13f2a16a43112afcd6e1bcc29c418176894d53"
	// Orphan commit on orphan-poison branch (no common ancestor with main)
	orphanSHA = "614a37a63d1a75476792a8781b55983a9d9bcb80"
	// A SHA that doesn't exist anywhere
	fakeSHA = "0000000000000000000000000000000000000000"
	// Commit on choam-io/actions-test-fixtures-fork attacker-payload branch
	// This SHA exists in the fork network but NOT in the upstream bare clone
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
	// Use a temp dir so integration tests don't pollute the real cache
	r.CacheDir = t.TempDir()
	return r
}

// TestIntegration_Reachable_HeadSHA verifies that the HEAD commit (where v1
// points) is reported as reachable via git merge-base.
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
// git-based reachability is superior to the GitHub compare API.
//
// The compare API treats the entire fork network as one graph, so a commit
// pushed to choam-io/actions-test-fixtures-fork is visible via the upstream
// compare endpoint and appears "reachable" (behind). This is a false negative.
//
// A bare clone of upstream-only excludes fork objects, so the attacker SHA
// from the fork won't exist in the clone → detected as UNREACHABLE.
func TestIntegration_Unreachable_ForkNetworkInjection(t *testing.T) {
	skipWithoutAuth(t)
	r := newLiveResolver(t)

	result := r.CheckReachability(fixtureOwner, fixtureRepo, forkAttackerSHA, "v1")
	assert.Equal(t, Unreachable, result.Status,
		"fork-network SHA should NOT be reachable from upstream bare clone: %+v", result)
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
