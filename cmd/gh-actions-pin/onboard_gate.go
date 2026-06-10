package main

import (
	"fmt"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-pin/internal/pipeline/checks"
	"github.com/spf13/cobra"
)

// noOnboardFlag reports the value of the persistent --no-onboard flag,
// defaulting to false when it is not registered.
func noOnboardFlag(cmd *cobra.Command) bool {
	v, _ := cmd.Flags().GetBool("no-onboard")
	return v
}

// gateNoOnboard enforces --no-onboard on a diagnosed report.
//
// A direct uses: ref with no lockfile entry for its own workflow surfaces
// as a NotPinned finding: the known-action set in checkNotPinned is scoped
// per workflow (run.go looks up only that workflow's recorded deps), so
// NotPinned fires exactly at the onboarding boundary — a brand-new
// workflow, or a new action inside an already-tracked one. Each such
// finding is rewritten to OnboardingRequired and its ref dropped from the
// workflow's ActionRefs, so the Plan pass never resolves or pins it.
//
// An already-tracked action whose ref was bumped surfaces as RefChanged
// instead (its NWO is in the workflow's recorded deps at a different ref).
// Those are left untouched and still re-pin. Returns the number of refs
// refused.
func gateNoOnboard(report *checks.Report) int {
	refused := 0
	for wi := range report.Workflows {
		wr := &report.Workflows[wi]
		refusedKeys := make(map[string]bool)
		for fi := range wr.Findings {
			f := &wr.Findings[fi]
			if f.Category != checks.NotPinned || f.ActionRef == nil {
				continue
			}
			ar := f.ActionRef
			refusedKeys[parserlock.IndexKey(ar.Owner, ar.Repo, ar.Ref)] = true
			f.Category = checks.OnboardingRequired
			f.Detail = fmt.Sprintf("%s@%s has no lockfile entry; --no-onboard refuses to add new workflows or actions", ar.FullName(), ar.Ref)
			f.Remediation = "onboard it first with `gh actions-pin check` (without --no-onboard)"
			refused++
		}
		if len(refusedKeys) == 0 {
			continue
		}
		kept := make([]parserlock.ActionRef, 0, len(wr.ActionRefs))
		for _, ar := range wr.ActionRefs {
			if refusedKeys[parserlock.IndexKey(ar.Owner, ar.Repo, ar.Ref)] {
				continue
			}
			kept = append(kept, ar)
		}
		wr.ActionRefs = kept
	}
	return refused
}
