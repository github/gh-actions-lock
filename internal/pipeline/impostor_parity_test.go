package pipeline

import (
	"testing"

	"github.com/github/gh-actions-lock/internal/pipeline/checks"

	"github.com/github/gh-actions-lock/internal/dep"
	"github.com/github/gh-actions-lock/internal/ghapi"
	"github.com/github/gh-actions-lock/internal/resolve"
)

const (
	testShaImpostor   = "ffffffffffffffffffffffffffffffffffffffff"
	testShaCheckoutV4 = "8e8c483db84b4bee98b60c0593521ed34d9990e8"
	testShaCheckoutV3 = "11bd71901bbe5b1630ceea73d27597364c9af683"
	testShaSetupGoV5  = "0aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

// TestLiveReachImpostorFindings_Parity proves the pre-pin live-direct sweep
// emits checks.ImpostorCommit for every shape that today's post-pin
// alertImpostor sweep would catch (and only those shapes). Each case is the
// pre-condition for deleting AutoFixAlertedImposters from apply.go.
func TestLiveReachImpostorFindings_Parity(t *testing.T) {
	const wfPath = ".github/workflows/ci.yml"

	directNWO := map[ghapi.Repo]bool{
		ghapi.ForRepo("actions", "checkout"): true,
	}
	transitiveDirectNWO := map[ghapi.Repo]bool{
		ghapi.ForRepo("actions", "cache"): true,
		// someorg/helper is transitive, no entry here
	}

	tests := []struct {
		name          string
		reach         []resolve.ReachabilityResult
		live          []dep.Dependency
		directNWOs    map[ghapi.Repo]bool
		parentMap     map[string][]string
		existing      []checks.Finding
		wantCount     int
		wantCategory  checks.Category
		wantParentSet bool
		wantSHA       string
	}{
		{
			name: "unpinned direct ref resolves to unreachable SHA",
			reach: []resolve.ReachabilityResult{{
				Owner: "actions", Repo: "checkout", Ref: "v4", SHA: testShaImpostor,
				DepKey: "actions/checkout@v4",
				Status: resolve.Unreachable,
				Detail: "no branch contains commit",
			}},
			live: []dep.Dependency{
				{NWO: "actions/checkout", Ref: "v4", SHA: testShaImpostor},
			},
			directNWOs:   directNWO,
			wantCount:    1,
			wantCategory: checks.ImpostorCommit,
			wantSHA:      testShaImpostor,
		},
		{
			name: "unpinned transitive dep (different NWO) resolves to unreachable SHA",
			reach: []resolve.ReachabilityResult{{
				Owner: "someorg", Repo: "helper", Ref: "v1", SHA: testShaImpostor,
				DepKey: "someorg/helper@v1",
				Status: resolve.Unreachable,
			}},
			live: []dep.Dependency{
				{NWO: "someorg/helper", Ref: "v1", SHA: testShaImpostor},
			},
			directNWOs: transitiveDirectNWO,
			parentMap: map[string][]string{
				"someorg/helper@v1": {"actions/cache@v4"},
			},
			wantCount:     1,
			wantCategory:  checks.ImpostorCommit,
			wantParentSet: true,
			wantSHA:       testShaImpostor,
		},
		{
			name: "reachable live SHA emits nothing",
			reach: []resolve.ReachabilityResult{{
				Owner: "actions", Repo: "checkout", Ref: "v4", SHA: testShaCheckoutV4,
				DepKey: "actions/checkout@v4",
				Status: resolve.Reachable,
			}},
			live: []dep.Dependency{
				{NWO: "actions/checkout", Ref: "v4", SHA: testShaCheckoutV4},
			},
			directNWOs: directNWO,
			wantCount:  0,
		},
		{
			name: "suppressed when prior checks.ImpostorCommit already covers dep",
			reach: []resolve.ReachabilityResult{{
				Owner: "actions", Repo: "checkout", Ref: "v4", SHA: testShaImpostor,
				DepKey: "actions/checkout@v4",
				Status: resolve.Unreachable,
			}},
			live: []dep.Dependency{
				{NWO: "actions/checkout", Ref: "v4", SHA: testShaImpostor},
			},
			directNWOs: directNWO,
			existing: []checks.Finding{{
				WorkflowPath: wfPath,
				Category:     checks.ImpostorCommit,
				Dependency:   &dep.Dependency{NWO: "actions/checkout", Ref: "v4", SHA: testShaImpostor},
			}},
			wantCount: 0,
		},
		{
			name: "suppressed when prior checks.LockfileForgery covers dep",
			reach: []resolve.ReachabilityResult{{
				Owner: "actions", Repo: "checkout", Ref: "v4", SHA: testShaImpostor,
				DepKey: "actions/checkout@v4",
				Status: resolve.Unreachable,
			}},
			live: []dep.Dependency{
				{NWO: "actions/checkout", Ref: "v4", SHA: testShaImpostor},
			},
			directNWOs: directNWO,
			existing: []checks.Finding{{
				WorkflowPath: wfPath,
				Category:     checks.LockfileForgery,
				Dependency:   &dep.Dependency{NWO: "actions/checkout", Ref: "v4", SHA: testShaImpostor},
			}},
			wantCount: 0,
		},
		{
			name: "unknown status emits nothing (only Unreachable fires)",
			reach: []resolve.ReachabilityResult{{
				Owner: "actions", Repo: "checkout", Ref: "v4", SHA: testShaImpostor,
				DepKey: "actions/checkout@v4",
				Status: resolve.ReachabilityUnknown,
			}},
			live: []dep.Dependency{
				{NWO: "actions/checkout", Ref: "v4", SHA: testShaImpostor},
			},
			directNWOs: directNWO,
			wantCount:  0,
		},
		{
			name: "deduplicates second reach result for same dep",
			reach: []resolve.ReachabilityResult{
				{
					Owner: "actions", Repo: "checkout", Ref: "v4", SHA: testShaImpostor,
					DepKey: "actions/checkout@v4",
					Status: resolve.Unreachable,
				},
				{
					Owner: "actions", Repo: "checkout", Ref: "v4", SHA: testShaImpostor,
					DepKey: "actions/checkout@v4",
					Status: resolve.Unreachable,
				},
			},
			live: []dep.Dependency{
				{NWO: "actions/checkout", Ref: "v4", SHA: testShaImpostor},
			},
			directNWOs:   directNWO,
			wantCount:    1,
			wantCategory: checks.ImpostorCommit,
			wantSHA:      testShaImpostor,
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
			if f.Severity != checks.SeverityError {
				t.Errorf("severity = %s, want %s", f.Severity, checks.SeverityError)
			}
			if f.Confidence != checks.ConfidenceHigh {
				t.Errorf("confidence = %s, want %s", f.Confidence, checks.ConfidenceHigh)
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
		existing  []dep.Dependency
		live      []dep.Dependency
		wantCount int
	}{
		{
			name:     "unpinned: all live deps need a fresh check",
			existing: nil,
			live: []dep.Dependency{
				{NWO: "actions/checkout", Ref: "v4", SHA: testShaCheckoutV4},
				{NWO: "actions/setup-go", Ref: "v5", SHA: testShaSetupGoV5},
			},
			wantCount: 2,
		},
		{
			name: "existing locked SHA covers reach key — skipped",
			existing: []dep.Dependency{
				{NWO: "actions/checkout", Ref: "v4", SHA: testShaCheckoutV4},
			},
			live: []dep.Dependency{
				{NWO: "actions/checkout", Ref: "v4", SHA: testShaCheckoutV4},
			},
			wantCount: 0,
		},
		{
			name: "live-moved: existing at same dep key but different SHA — skipped (live-moved sweep handles it)",
			existing: []dep.Dependency{
				{NWO: "actions/checkout", Ref: "v4", SHA: testShaCheckoutV3},
			},
			live: []dep.Dependency{
				{NWO: "actions/checkout", Ref: "v4", SHA: testShaCheckoutV4},
			},
			wantCount: 0,
		},
		{
			name: "partial coverage: existing covers one, live-extra needs check",
			existing: []dep.Dependency{
				{NWO: "actions/checkout", Ref: "v4", SHA: testShaCheckoutV4},
			},
			live: []dep.Dependency{
				{NWO: "actions/checkout", Ref: "v4", SHA: testShaCheckoutV4},
				{NWO: "actions/setup-go", Ref: "v5", SHA: testShaSetupGoV5},
			},
			wantCount: 1,
		},
		{
			name:     "dedups by reach key",
			existing: nil,
			live: []dep.Dependency{
				{NWO: "actions/checkout", Ref: "v4", SHA: testShaCheckoutV4},
				{NWO: "actions/checkout", Ref: "v4", SHA: testShaCheckoutV4},
			},
			wantCount: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pw := checks.ParsedWorkflow{Path: "wf", ExistingDeps: tc.existing}
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
	parsed := []checks.ParsedWorkflow{
		{Path: "a.yml", ExistingDeps: nil},
		{Path: "b.yml", ExistingDeps: nil},
	}
	live := []dep.Dependency{
		{NWO: "actions/checkout", Ref: "v4", SHA: testShaCheckoutV4},
		{NWO: "actions/setup-go", Ref: "v5", SHA: testShaSetupGoV5},
	}
	got := CollectLiveDirectReachDeps(parsed, live)
	if len(got) != 2 {
		t.Fatalf("got %d deps, want 2: %+v", len(got), got)
	}

	// With one workflow already pinning checkout, only setup-go remains.
	parsed[0].ExistingDeps = []dep.Dependency{
		{NWO: "actions/checkout", Ref: "v4", SHA: testShaCheckoutV4},
	}
	got = CollectLiveDirectReachDeps(parsed, live)
	if len(got) != 1 || got[0].NWO != "actions/setup-go" {
		t.Fatalf("got %+v, want only setup-go", got)
	}
}
