package doctor

import (
	"testing"

	"github.com/github/gh-actions-pin/internal/cachekey"
	"github.com/github/gh-actions-pin/internal/lockfile"
	"github.com/github/gh-actions-pin/internal/resolve"
)

// TestLiveReachImpostorFindings_Parity proves the pre-pin live-direct sweep
// emits CategoryImpostorCommit for every shape that today's post-pin
// alertImpostor sweep would catch (and only those shapes). Each case is the
// pre-condition for deleting AutoFixAlertedImposters from apply.go.
func TestLiveReachImpostorFindings_Parity(t *testing.T) {
	const wfPath = ".github/workflows/ci.yml"

	directNWO := map[cachekey.Repo]bool{
		cachekey.ForRepo("actions", "checkout"): true,
	}
	transitiveDirectNWO := map[cachekey.Repo]bool{
		cachekey.ForRepo("actions", "cache"): true,
		// someorg/helper is transitive, no entry here
	}

	tests := []struct {
		name          string
		reach         []resolve.ReachabilityResult
		live          []lockfile.Dependency
		directNWOs    map[cachekey.Repo]bool
		parentMap     map[string][]string
		existing      []Finding
		wantCount     int
		wantCategory  Category
		wantParentSet bool
		wantSHA       string
	}{
		{
			name: "unpinned direct ref resolves to unreachable SHA",
			reach: []resolve.ReachabilityResult{{
				Owner: "actions", Repo: "checkout", Ref: "v4", SHA: shaImpostor,
				DepKey: "actions/checkout@v4",
				Status: resolve.Unreachable,
				Detail: "no branch contains commit",
			}},
			live: []lockfile.Dependency{
				{NWO: "actions/checkout", Ref: "v4", SHA: shaImpostor},
			},
			directNWOs:   directNWO,
			wantCount:    1,
			wantCategory: CategoryImpostorCommit,
			wantSHA:      shaImpostor,
		},
		{
			name: "unpinned transitive dep (different NWO) resolves to unreachable SHA",
			reach: []resolve.ReachabilityResult{{
				Owner: "someorg", Repo: "helper", Ref: "v1", SHA: shaImpostor,
				DepKey: "someorg/helper@v1",
				Status: resolve.Unreachable,
			}},
			live: []lockfile.Dependency{
				{NWO: "someorg/helper", Ref: "v1", SHA: shaImpostor},
			},
			directNWOs: transitiveDirectNWO,
			parentMap: map[string][]string{
				"someorg/helper@v1": {"actions/cache@v4"},
			},
			wantCount:     1,
			wantCategory:  CategoryImpostorCommit,
			wantParentSet: true,
			wantSHA:       shaImpostor,
		},
		{
			name: "reachable live SHA emits nothing",
			reach: []resolve.ReachabilityResult{{
				Owner: "actions", Repo: "checkout", Ref: "v4", SHA: shaCheckoutV4,
				DepKey: "actions/checkout@v4",
				Status: resolve.Reachable,
			}},
			live: []lockfile.Dependency{
				{NWO: "actions/checkout", Ref: "v4", SHA: shaCheckoutV4},
			},
			directNWOs: directNWO,
			wantCount:  0,
		},
		{
			name: "suppressed when prior CategoryImpostorCommit already covers dep",
			reach: []resolve.ReachabilityResult{{
				Owner: "actions", Repo: "checkout", Ref: "v4", SHA: shaImpostor,
				DepKey: "actions/checkout@v4",
				Status: resolve.Unreachable,
			}},
			live: []lockfile.Dependency{
				{NWO: "actions/checkout", Ref: "v4", SHA: shaImpostor},
			},
			directNWOs: directNWO,
			existing: []Finding{{
				WorkflowPath: wfPath,
				Category:     CategoryImpostorCommit,
				Dependency:   &lockfile.Dependency{NWO: "actions/checkout", Ref: "v4", SHA: shaImpostor},
			}},
			wantCount: 0,
		},
		{
			name: "suppressed when prior CategoryLockfileForgery covers dep",
			reach: []resolve.ReachabilityResult{{
				Owner: "actions", Repo: "checkout", Ref: "v4", SHA: shaImpostor,
				DepKey: "actions/checkout@v4",
				Status: resolve.Unreachable,
			}},
			live: []lockfile.Dependency{
				{NWO: "actions/checkout", Ref: "v4", SHA: shaImpostor},
			},
			directNWOs: directNWO,
			existing: []Finding{{
				WorkflowPath: wfPath,
				Category:     CategoryLockfileForgery,
				Dependency:   &lockfile.Dependency{NWO: "actions/checkout", Ref: "v4", SHA: shaImpostor},
			}},
			wantCount: 0,
		},
		{
			name: "unknown status emits nothing (only Unreachable fires)",
			reach: []resolve.ReachabilityResult{{
				Owner: "actions", Repo: "checkout", Ref: "v4", SHA: shaImpostor,
				DepKey: "actions/checkout@v4",
				Status: resolve.ReachabilityUnknown,
			}},
			live: []lockfile.Dependency{
				{NWO: "actions/checkout", Ref: "v4", SHA: shaImpostor},
			},
			directNWOs: directNWO,
			wantCount:  0,
		},
		{
			name: "deduplicates second reach result for same dep",
			reach: []resolve.ReachabilityResult{
				{
					Owner: "actions", Repo: "checkout", Ref: "v4", SHA: shaImpostor,
					DepKey: "actions/checkout@v4",
					Status: resolve.Unreachable,
				},
				{
					Owner: "actions", Repo: "checkout", Ref: "v4", SHA: shaImpostor,
					DepKey: "actions/checkout@v4",
					Status: resolve.Unreachable,
				},
			},
			live: []lockfile.Dependency{
				{NWO: "actions/checkout", Ref: "v4", SHA: shaImpostor},
			},
			directNWOs:   directNWO,
			wantCount:    1,
			wantCategory: CategoryImpostorCommit,
			wantSHA:      shaImpostor,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := liveReachImpostorFindings(wfPath, tc.reach, tc.live, tc.directNWOs, tc.parentMap, tc.existing)
			if len(got) != tc.wantCount {
				t.Fatalf("got %d findings, want %d: %+v", len(got), tc.wantCount, got)
			}
			if tc.wantCount == 0 {
				return
			}
			f := got[0]
			if f.Category != tc.wantCategory {
				t.Errorf("category = %s, want %s", f.Category, tc.wantCategory)
			}
			if f.Severity != SeverityError {
				t.Errorf("severity = %s, want %s", f.Severity, SeverityError)
			}
			if f.Confidence != ConfidenceHigh {
				t.Errorf("confidence = %s, want %s", f.Confidence, ConfidenceHigh)
			}
			if f.Dependency == nil || f.Dependency.SHA != tc.wantSHA {
				t.Errorf("dependency SHA = %v, want %s", f.Dependency, tc.wantSHA)
			}
			if tc.wantParentSet && f.ParentNWO == "" {
				t.Error("expected ParentNWO to be set for transitive case")
			}
			if !tc.wantParentSet && f.ParentNWO != "" {
				t.Errorf("ParentNWO = %q, want empty for direct case", f.ParentNWO)
			}
		})
	}
}

// TestLiveDirectReachDeps_Coverage proves the synthesis function:
//   - emits nothing when every live dep is already covered by ExistingDeps
//   - emits nothing when every live dep is covered by the live-moved sweep
//     (existing dep at the same key, different SHA)
//   - emits one entry per uncovered live dep, deduped by (NWO, ref, SHA)
func TestLiveDirectReachDeps_Coverage(t *testing.T) {
	tests := []struct {
		name      string
		existing  []lockfile.Dependency
		live      []lockfile.Dependency
		wantCount int
	}{
		{
			name:     "unpinned: all live deps need a fresh check",
			existing: nil,
			live: []lockfile.Dependency{
				{NWO: "actions/checkout", Ref: "v4", SHA: shaCheckoutV4},
				{NWO: "actions/setup-go", Ref: "v5", SHA: shaSetupGoV5},
			},
			wantCount: 2,
		},
		{
			name: "existing locked SHA covers reach key — skipped",
			existing: []lockfile.Dependency{
				{NWO: "actions/checkout", Ref: "v4", SHA: shaCheckoutV4},
			},
			live: []lockfile.Dependency{
				{NWO: "actions/checkout", Ref: "v4", SHA: shaCheckoutV4},
			},
			wantCount: 0,
		},
		{
			name: "live-moved: existing at same dep key but different SHA — skipped (live-moved sweep handles it)",
			existing: []lockfile.Dependency{
				{NWO: "actions/checkout", Ref: "v4", SHA: shaCheckoutV3},
			},
			live: []lockfile.Dependency{
				{NWO: "actions/checkout", Ref: "v4", SHA: shaCheckoutV4},
			},
			wantCount: 0,
		},
		{
			name: "partial coverage: existing covers one, live-extra needs check",
			existing: []lockfile.Dependency{
				{NWO: "actions/checkout", Ref: "v4", SHA: shaCheckoutV4},
			},
			live: []lockfile.Dependency{
				{NWO: "actions/checkout", Ref: "v4", SHA: shaCheckoutV4},
				{NWO: "actions/setup-go", Ref: "v5", SHA: shaSetupGoV5},
			},
			wantCount: 1,
		},
		{
			name:     "dedups by reach key",
			existing: nil,
			live: []lockfile.Dependency{
				{NWO: "actions/checkout", Ref: "v4", SHA: shaCheckoutV4},
				{NWO: "actions/checkout", Ref: "v4", SHA: shaCheckoutV4},
			},
			wantCount: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pw := ParsedWorkflow{Path: "wf", ExistingDeps: tc.existing}
			got := liveDirectReachDeps(pw, tc.live)
			if len(got) != tc.wantCount {
				t.Fatalf("got %d deps, want %d: %+v", len(got), tc.wantCount, got)
			}
		})
	}
}

// TestCollectLiveDirectReachDeps_UnionDedup proves the cmd-level pre-warm
// helper unions per-workflow results and dedupes across workflows.
func TestCollectLiveDirectReachDeps_UnionDedup(t *testing.T) {
	parsed := []ParsedWorkflow{
		{Path: "a.yml", ExistingDeps: nil},
		{Path: "b.yml", ExistingDeps: nil},
	}
	live := []lockfile.Dependency{
		{NWO: "actions/checkout", Ref: "v4", SHA: shaCheckoutV4},
		{NWO: "actions/setup-go", Ref: "v5", SHA: shaSetupGoV5},
	}
	got := CollectLiveDirectReachDeps(parsed, live)
	if len(got) != 2 {
		t.Fatalf("got %d deps, want 2: %+v", len(got), got)
	}

	// With one workflow already pinning checkout, only setup-go remains.
	parsed[0].ExistingDeps = []lockfile.Dependency{
		{NWO: "actions/checkout", Ref: "v4", SHA: shaCheckoutV4},
	}
	got = CollectLiveDirectReachDeps(parsed, live)
	if len(got) != 1 || got[0].NWO != "actions/setup-go" {
		t.Fatalf("got %+v, want only setup-go", got)
	}
}
