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

// gateNoOnboard rewrites per-workflow NotPinned findings to OnboardingRequired
// and drops their refs so Plan never pins them. Returns the refused entry labels
// (e.g. "actions/checkout@v4 in .github/workflows/ci.yml").
func gateNoOnboard(report *checks.Report) []string {
	var refused []string
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
			refused = append(refused, fmt.Sprintf("%s@%s in %s", ar.FullName(), ar.Ref, wr.Path))
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
