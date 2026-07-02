package pipeline

import (
	"context"
	"fmt"

	"github.com/github/gh-actions-lock/internal/lockfile"
	"github.com/github/gh-actions-lock/internal/pipeline/checks"
)

// VerifyLocalCoverage performs a zero-network static check: every action ref
// in the parsed workflows must have a matching lockfile entry. Returns a
// Report with NotPinned findings for any uncovered refs, suitable for rendering
// with the standard terminal or JSON formatters.
func VerifyLocalCoverage(parsed []checks.ParsedWorkflow, store *lockfile.State) *checks.Report {
	lf := store.File()
	results := make([]checks.WorkflowReport, 0, len(parsed))

	for _, pw := range parsed {
		wr := checks.WorkflowReport{
			Path:       pw.Path,
			ActionRefs: pw.Refs,
			Deps:       pw.ExistingDeps,
		}

		if pw.LoadErr != nil {
			wr.Findings = append(wr.Findings, checks.Finding{
				WorkflowPath: pw.Path,
				Category:     checks.NotPinned,
				Severity:     checks.SeverityError,
				Confidence:   checks.ConfidenceHigh,
				Detail:       fmt.Sprintf("could not parse workflow: %v", pw.LoadErr),
			})
			results = append(results, wr)
			continue
		}

		if pw.DepsErr != nil {
			wr.Findings = append(wr.Findings, checks.Finding{
				WorkflowPath: pw.Path,
				Category:     checks.NotPinned,
				Severity:     checks.SeverityError,
				Confidence:   checks.ConfidenceHigh,
				Detail:       fmt.Sprintf("failed to read dependencies: %s", pw.DepsErr),
				Remediation:  "fix or regenerate the dependencies: section with `gh actions-lock`",
			})
			results = append(results, wr)
			continue
		}

		if len(pw.Refs) == 0 && len(pw.LocalPaths) == 0 {
			results = append(results, wr)
			continue
		}

		// Use RunChecks with a nil resolver to get structural-only findings
		// (NotPinned, ShaAsRef, RefChanged, Stale). No network calls.
		findings := checks.RunChecks(context.Background(), pw, lf, nil)
		wr.Findings = findings
		results = append(results, wr)
	}

	return &checks.Report{Workflows: results}
}
