package pin

import (
	"testing"

	"github.com/github/gh-actions-lock/internal/dep"
	"github.com/github/gh-actions-lock/internal/pipeline/checks"
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

func TestNeedsRepin_RefMoved(t *testing.T) {
	refMovedOnly := checks.WorkflowReport{
		Findings: []checks.Finding{{Category: checks.RefMoved, Severity: checks.SeverityWarning}},
	}

	tests := []struct {
		name string
		opts PlanOptions
		want bool
	}{
		{name: "no flags: ref-moved does not repin", want: false},
		{name: "relock repins ref-moved", opts: PlanOptions{Relock: true}, want: true},
		{name: "accept-moved repins ref-moved", opts: PlanOptions{AcceptMoved: true}, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := needsRepin(refMovedOnly, tt.opts); got != tt.want {
				t.Errorf("needsRepin = %v, want %v", got, tt.want)
			}
		})
	}
}

// A workflow with an unreachable-pin already NeedsAttention, so it always
// re-pins regardless of flags — but relock must not turn it into an accepted
// (silently re-pinned) fix; that gate lives in reportHasUnfixableErrors.
func TestNeedsRepin_UnreachablePinAlwaysRepins(t *testing.T) {
	wr := checks.WorkflowReport{
		Findings: []checks.Finding{{Category: checks.UnreachablePin, Severity: checks.SeverityError}},
	}
	if !needsRepin(wr, PlanOptions{}) {
		t.Errorf("unreachable-pin should always need re-pin via NeedsAttention")
	}
}
