package pipeline

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/dep"
	"github.com/github/gh-actions-lock/internal/lockfile"
	"github.com/github/gh-actions-lock/internal/pipeline/checks"
	"github.com/github/gh-actions-lock/internal/resolve"
)

func checkLockedManifests(ctx context.Context, r *resolve.Resolver, parsed []checks.ParsedWorkflow, store *lockfile.State, report *checks.Report) error {
	file := store.File()
	deps := store.AllDeps()
	for i, pw := range parsed {
		if isBlocked(pw) || report.Workflows[i].SkipCommit || !report.Workflows[i].IsValid() {
			continue
		}
		recorded, _ := pw.PartitionRefs()
		for i, ref := range recorded {
			if !parserlock.IsFullSha(ref.Ref) {
				continue
			}
			for _, d := range pw.ExistingDeps {
				if strings.EqualFold(d.NWO, ref.NWO()) && strings.EqualFold(d.SHA, ref.Ref) {
					recorded[i].Ref = d.Ref
					break
				}
			}
		}
		_, parents, warnings, err := r.ResolveWithWarnings(ctx, recorded, deps)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		wr := &report.Workflows[i]
		appendResolutionWarnings(wr, warnings)
		// The recorded edges must include every edge discovered at the locked
		// commits, even when a child exists elsewhere in the same lockfile.
		for _, child := range slices.Sorted(maps.Keys(parents)) {
			pp := parents[child]
			for _, parent := range pp {
				action, ok := file.Dependencies[foldKey(parent)]
				if !ok {
					continue
				}
				found := false
				for _, use := range action.Uses {
					if foldKey(use) == foldKey(child) {
						found = true
						break
					}
				}
				if found {
					continue
				}
				ref := parserlock.ParseActionRef(child)
				f := checks.Finding{
					WorkflowPath:       pw.Path,
					Category:           checks.NotPinned,
					Severity:           checks.SeverityError,
					Confidence:         checks.ConfidenceHigh,
					ParentNWO:          parent,
					IncompleteLockfile: true,
					TrackedAction:      true,
					Detail:             fmt.Sprintf("locked manifest for %s uses %s but the lockfile omits that edge", parent, child),
					Remediation:        "restore the graph from version control, or run `gh actions-lock --relock`",
				}
				if ref != nil {
					f.Dependency = &dep.Dependency{NWO: ref.NWO(), Ref: ref.Ref}
				}
				wr.Findings = append(dropValid(wr.Findings), f)
				wr.SkipCommit = true
			}
		}
		if err != nil && !(wr.SkipCommit && onlyUnlockedRefs(err)) {
			wr.Findings = append(dropValid(wr.Findings), checks.Finding{
				WorkflowPath: pw.Path,
				Category:     checks.ReachabilityUnknown,
				Severity:     checks.SeverityError,
				Confidence:   checks.ConfidenceLow,
				Detail:       fmt.Sprintf("could not verify locked dependency manifests: %s", err),
				Remediation:  "retry; complete dependency coverage could not be established",
			})
			wr.SkipCommit = true
		}
		// Direct missing entries are diagnosed structurally. Unlocked children
		// are named by the missing-edge findings above.
	}
	return nil
}

func onlyUnlockedRefs(err error) bool {
	if err == nil {
		return true
	}
	if _, ok := err.(*resolve.UnlockedRefError); ok {
		return true
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, inner := range joined.Unwrap() {
			if !onlyUnlockedRefs(inner) {
				return false
			}
		}
		return true
	}
	if inner := errors.Unwrap(err); inner != nil {
		return onlyUnlockedRefs(inner)
	}
	return false
}
