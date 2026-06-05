package doctor

import (
	"testing"

	"github.com/github/gh-actions-pin/internal/lockfile"
	"github.com/github/gh-actions-pin/internal/resolver"
)

// TestPrewarmedResolver_LockedAndLiveCoexist guards the wiring that
// powers the tag-hijacked-to-fork-network impostor-commit detection:
// the same NWO@Ref dep can have a Reachable result for the LOCKED SHA
// and an Unreachable result for the OBSERVED SHA, and both must
// survive in the reach cache. The cache key includes the SHA, so
// collisions only happen when callers (e.g.
// reachabilityComplementFindings) key on Dependency.Key — that's why
// diagnose.go keeps the locked- and observed-SHA sweeps in separate
// slices.
func TestPrewarmedResolver_LockedAndLiveCoexist(t *testing.T) {
	const (
		owner  = "owner"
		repo   = "repo"
		ref    = "tampered"
		locked = "ea53476fdc172d8552df5af9658a45a367e4f41d"
		live   = "7b403c9ec14b00000000000000000000deadbeef"
	)
	locks := []resolver.ReachabilityResult{
		{Owner: owner, Repo: repo, Ref: ref, SHA: locked, Status: resolver.Reachable},
	}
	lives := []resolver.ReachabilityResult{
		{Owner: owner, Repo: repo, Ref: ref, SHA: live, Status: resolver.Unreachable},
	}
	pw := newPrewarmedResolver(nil, nil, locks, lives)
	if got := pw.CheckReachability(owner, repo, locked, ref); got != resolver.Reachable {
		t.Errorf("locked SHA: got %v, want Reachable", got)
	}
	if got := pw.CheckReachability(owner, repo, live, ref); got != resolver.Unreachable {
		t.Errorf("observed SHA: got %v, want Unreachable", got)
	}
}

func TestCollectLiveMovedReachDeps(t *testing.T) {
	mkDep := func(nwo, ref, sha string) lockfile.Dependency {
		return lockfile.Dependency{NWO: nwo, Ref: ref, SHA: sha}
	}
	existing := []lockfile.Dependency{
		mkDep("owner/repo", "v4", "aaaa000000000000000000000000000000000000"),   // moved → in output
		mkDep("owner/repo", "v3", "bbbb000000000000000000000000000000000000"),   // unchanged → skipped
		mkDep("owner/repo", "v5", "cccc000000000000000000000000000000000000"),   // no live entry → skipped
		mkDep("owner/repo", "main", "dddd000000000000000000000000000000000000"), // moved → in output
		mkDep("owner/repo", "main", "dddd000000000000000000000000000000000000"), // dup → dedup'd
	}
	live := []lockfile.Dependency{
		mkDep("owner/repo", "v4", "1111000000000000000000000000000000000000"),
		mkDep("owner/repo", "v3", "bbbb000000000000000000000000000000000000"),
		mkDep("owner/repo", "main", "2222000000000000000000000000000000000000"),
	}
	parsed := []ParsedWorkflow{{Path: ".github/workflows/a.yml", ExistingDeps: existing}}
	got := CollectLiveMovedReachDeps(parsed, live)

	if len(got) != 2 {
		t.Fatalf("got %d synthetic deps, want 2: %+v", len(got), got)
	}
	wantSHAs := map[string]bool{
		"1111000000000000000000000000000000000000": false,
		"2222000000000000000000000000000000000000": false,
	}
	for _, d := range got {
		if d.Ref == "" || d.SHA == "" {
			t.Errorf("synthetic dep missing fields: %#v", d)
		}
		if _, ok := wantSHAs[d.SHA]; !ok {
			t.Errorf("unexpected SHA in output: %s", d.SHA)
			continue
		}
		wantSHAs[d.SHA] = true
	}
	for sha, seen := range wantSHAs {
		if !seen {
			t.Errorf("expected live SHA %s in output, missing", sha)
		}
	}
}

// TestLiveMovedDeps mirrors TestCollectLiveMovedReachDeps for the
// per-workflow path used inside diagnoseOneParsed.
func TestLiveMovedDeps(t *testing.T) {
	mkDep := func(nwo, ref, sha string) lockfile.Dependency {
		return lockfile.Dependency{NWO: nwo, Ref: ref, SHA: sha}
	}
	existing := []lockfile.Dependency{
		mkDep("owner/repo", "v4", "aaaa000000000000000000000000000000000000"),
		mkDep("owner/repo", "v3", "bbbb000000000000000000000000000000000000"),
	}
	live := []lockfile.Dependency{
		mkDep("owner/repo", "v4", "1111000000000000000000000000000000000000"),
		mkDep("owner/repo", "v3", "bbbb000000000000000000000000000000000000"),
	}
	got := liveMovedDeps(existing, live)
	if len(got) != 1 {
		t.Fatalf("got %d synthetic deps, want 1: %+v", len(got), got)
	}
	if got[0].Ref != "v4" || got[0].SHA != "1111000000000000000000000000000000000000" {
		t.Errorf("unexpected synthetic dep: %#v", got[0])
	}
}
