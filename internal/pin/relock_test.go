package pin

import (
	"context"
	"testing"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/dep"
	"github.com/github/gh-actions-lock/internal/ghapi/httpmock"
	"github.com/github/gh-actions-lock/internal/pinpool"
	"github.com/github/gh-actions-lock/internal/pipeline/checks"
	"github.com/github/gh-actions-lock/internal/resolve"
	"github.com/github/gh-actions-lock/internal/tag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func inv(nwo, ref, sha string) checks.InventoryEntry {
	return checks.InventoryEntry{Dep: dep.Dependency{NWO: nwo, Ref: ref, SHA: sha}}
}

func movedFinding(cat checks.Category, nwo, ref, sha string) checks.Finding {
	return checks.Finding{
		Category:   cat,
		Dependency: &dep.Dependency{NWO: nwo, Ref: ref, SHA: sha},
	}
}

func hasDep(inventory []checks.InventoryEntry, nwo string) bool {
	for _, e := range inventory {
		if e.Dep.NWO == nwo {
			return true
		}
	}
	return false
}

func TestPruneStaleInventory_Relock(t *testing.T) {
	inventory := []checks.InventoryEntry{
		inv("octo/branch", "main", "aaaa"),
		inv("octo/unreach", "v1", "bbbb"),
		inv("octo/keep", "v2", "cccc"),
	}
	findings := []checks.Finding{
		movedFinding(checks.RefMoved, "octo/branch", "main", "aaaa"),
		movedFinding(checks.UnreachablePin, "octo/unreach", "v1", "bbbb"),
	}

	tests := []struct {
		name          string
		acceptMoved   bool
		relock        bool
		wantPrunedRef bool // octo/branch (ref-moved) pruned
		wantPrunedUnr bool // octo/unreach (unreachable-pin) pruned
	}{
		{name: "no flags prunes neither"},
		{name: "relock prunes ref-moved only", relock: true, wantPrunedRef: true},
		{name: "accept-moved prunes both", acceptMoved: true, wantPrunedRef: true, wantPrunedUnr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := pruneStaleInventory(inventory, findings, tt.acceptMoved, tt.relock)
			if hasDep(got, "octo/branch") == tt.wantPrunedRef {
				t.Errorf("ref-moved dep present=%v, want pruned=%v", hasDep(got, "octo/branch"), tt.wantPrunedRef)
			}
			if hasDep(got, "octo/unreach") == tt.wantPrunedUnr {
				t.Errorf("unreachable-pin dep present=%v, want pruned=%v", hasDep(got, "octo/unreach"), tt.wantPrunedUnr)
			}
			if !hasDep(got, "octo/keep") {
				t.Errorf("unrelated dep octo/keep must never be pruned")
			}
		})
	}
}

// A moved *transitive* dep is pruned from inventory but is not a direct
// ActionRef, so the per-dep trust fast path would silently keep its stale SHA.
// Under --relock, planWorkflow must force the direct roots through recursive
// resolution and bump the transitive to its current SHA. Covers the reviewer
// request to exercise a moved transitive in a resolution test.
func TestPlanWorkflow_Relock_BumpsMovedTransitive(t *testing.T) {
	compSHA := "1111111111111111111111111111111111111111"
	oldTransSHA := "2222222222222222222222222222222222222222"
	newTransSHA := "3333333333333333333333333333333333333333"
	resolver, pool, tagger := newTransitivePlanFixture(t, compSHA, newTransSHA)

	wr := checks.WorkflowReport{
		Path: ".github/workflows/test.yml",
		ActionRefs: []parserlock.ActionRef{
			{Owner: "comp", Repo: "action", Ref: "v1.0.0"},
		},
		Inventory: []checks.InventoryEntry{
			{Dep: dep.Dependency{NWO: "comp/action", Ref: "v1.0.0", SHA: compSHA}, Direct: true},
			{Dep: dep.Dependency{NWO: "trans/dep", Ref: "v2", SHA: oldTransSHA}, Parents: []string{"comp/action@v1.0.0"}},
		},
		Findings: []checks.Finding{
			movedFinding(checks.RefMoved, "trans/dep", "v2", oldTransSHA),
		},
	}

	opts := PlanOptions{
		Resolver: resolver,
		Pool:     pool,
		Tagger:   tagger,
		Relock:   true,
	}

	result, err := planWorkflow(context.Background(), wr, opts, func(string) {})
	require.NoError(t, err)

	byNWO := make(map[string]Entry, len(result.entries))
	for _, e := range result.entries {
		byNWO[e.NWO] = e
	}

	trans, ok := byNWO["trans/dep"]
	require.True(t, ok, "transitive dep should be present")
	assert.Equal(t, newTransSHA, trans.SHA, "moved transitive must be bumped to its live SHA under --relock")
	assert.NotEqual(t, oldTransSHA, trans.SHA)
}

// Under --relock a workflow that still carries an unreachable-pin finding must
// be left untouched: re-resolving a moved sibling would pull the tampered
// (possibly transitive) pin through resolution and commit it at a live SHA
// before the hard-error gate. The empty httpmock registry proves no resolution
// happens — any network call would fail the test.
func TestPlanWorkflow_Relock_LeavesUnreachablePinUntouched(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)

	movedSHA := "1111111111111111111111111111111111111111"
	tamperedSHA := "2222222222222222222222222222222222222222"

	pool := pinpool.New(2, nil)
	resolver, err := resolve.New("github.com", pool, resolve.WithTransport(reg))
	require.NoError(t, err)

	wr := checks.WorkflowReport{
		Path: ".github/workflows/test.yml",
		ActionRefs: []parserlock.ActionRef{
			{Owner: "octo", Repo: "app", Ref: "main"},
			{Owner: "octo", Repo: "bad", Ref: "v1"},
		},
		Inventory: []checks.InventoryEntry{
			{Dep: dep.Dependency{NWO: "octo/app", Ref: "main", SHA: movedSHA}, Direct: true},
			{Dep: dep.Dependency{NWO: "octo/bad", Ref: "v1", SHA: tamperedSHA}, Direct: true},
		},
		Findings: []checks.Finding{
			movedFinding(checks.RefMoved, "octo/app", "main", movedSHA),
			movedFinding(checks.UnreachablePin, "octo/bad", "v1", tamperedSHA),
		},
	}

	opts := PlanOptions{
		Resolver: resolver,
		Pool:     pool,
		Tagger:   tag.NewListerForTest(t, reg),
		Relock:   true,
	}

	result, err := planWorkflow(context.Background(), wr, opts, func(string) {})
	require.NoError(t, err)

	byNWO := make(map[string]Entry, len(result.entries))
	for _, e := range result.entries {
		byNWO[e.NWO] = e
	}

	bad, ok := byNWO["octo/bad"]
	require.True(t, ok, "unreachable dep must be retained")
	assert.Equal(t, tamperedSHA, bad.SHA, "unreachable pin must not be rewritten under --relock")

	app, ok := byNWO["octo/app"]
	require.True(t, ok, "moved dep must be retained")
	assert.Equal(t, movedSHA, app.SHA, "moved ref must not be bumped while an unreachable pin blocks the run")
}
