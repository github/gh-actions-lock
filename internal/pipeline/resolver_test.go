package pipeline

import (
	"testing"

	"github.com/github/gh-actions-pin/internal/pipeline/checks"

	"github.com/github/gh-actions-pin/internal/dep"
	"github.com/github/gh-actions-pin/internal/resolve"
)

// TestPrewarmedResolver_LockedAndLiveCoexist verifies that locked-SHA
// and observed-SHA reach results for the same NWO@Ref both survive in
// the prewarmedResolver cache (the cache key includes the SHA).
func TestPrewarmedResolver_LockedAndLiveCoexist(t *testing.T) {
	const (
		owner  = "owner"
		repo   = "repo"
		ref    = "tampered"
		locked = "ea53476fdc172d8552df5af9658a45a367e4f41d"
		live   = "7b403c9ec14b00000000000000000000deadbeef"
	)
	locks := []resolve.ReachabilityResult{
		{Owner: owner, Repo: repo, Ref: ref, SHA: locked, Status: resolve.Reachable},
	}
	lives := []resolve.ReachabilityResult{
		{Owner: owner, Repo: repo, Ref: ref, SHA: live, Status: resolve.Unreachable},
	}
	pw := checks.NewPrewarmedResolver(nil, nil, locks, lives)
	if got := pw.CheckReachability(owner, repo, locked, ref); got != resolve.Reachable {
		t.Errorf("locked SHA: got %v, want Reachable", got)
	}
	if got := pw.CheckReachability(owner, repo, live, ref); got != resolve.Unreachable {
		t.Errorf("observed SHA: got %v, want Unreachable", got)
	}
}

func TestCollectLiveMovedReachDeps(t *testing.T) {
	mkDep := func(nwo, ref, sha string) dep.Dependency {
		return dep.Dependency{NWO: nwo, Ref: ref, SHA: sha}
	}
	existing := []dep.Dependency{
		mkDep("owner/repo", "v4", "aaaa000000000000000000000000000000000000"),   // moved → in output
		mkDep("owner/repo", "v3", "bbbb000000000000000000000000000000000000"),   // unchanged → skipped
		mkDep("owner/repo", "v5", "cccc000000000000000000000000000000000000"),   // no live entry → skipped
		mkDep("owner/repo", "main", "dddd000000000000000000000000000000000000"), // moved → in output
		mkDep("owner/repo", "main", "dddd000000000000000000000000000000000000"), // dup → dedup'd
	}
	live := []dep.Dependency{
		mkDep("owner/repo", "v4", "1111000000000000000000000000000000000000"),
		mkDep("owner/repo", "v3", "bbbb000000000000000000000000000000000000"),
		mkDep("owner/repo", "main", "2222000000000000000000000000000000000000"),
	}
	parsed := []checks.ParsedWorkflow{{Path: ".github/workflows/a.yml", ExistingDeps: existing}}
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
	mkDep := func(nwo, ref, sha string) dep.Dependency {
		return dep.Dependency{NWO: nwo, Ref: ref, SHA: sha}
	}
	existing := []dep.Dependency{
		mkDep("owner/repo", "v4", "aaaa000000000000000000000000000000000000"),
		mkDep("owner/repo", "v3", "bbbb000000000000000000000000000000000000"),
	}
	live := []dep.Dependency{
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
