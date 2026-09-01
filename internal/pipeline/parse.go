package pipeline

import (
	"context"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/ghapi"
	"github.com/github/gh-actions-lock/internal/lockfile"
	"github.com/github/gh-actions-lock/internal/pinpool"
	"github.com/github/gh-actions-lock/internal/pipeline/checks"
	"github.com/github/gh-actions-lock/internal/resolve"
	"github.com/github/gh-actions-lock/internal/workflowfile"
)

// Diagnose scans workflows and produces findings for each.
//
// It is a backward-compatible wrapper around ParseAll, resolver pre-warming,
// and DiagnoseParsed. Newer callers can drive those phases directly to control
// UI progress.
func Diagnose(ctx context.Context, paths []string, r *resolve.Resolver, store *lockfile.State, pool *pinpool.Pool) *checks.Report {
	parsed := ParseAll(paths, store)
	if r != nil {
		refs := CollectResolvable(parsed)
		if len(refs) > 0 {
			_, _, _ = r.ResolveAllRecursive(ctx, refs)
		}
	}
	return DiagnoseParsed(ctx, parsed, r, store, pool)
}

// ParseAll loads and parses every workflow path, returning a slice in input
// order. onScan, if non-nil, fires with 1-based progress before each workflow
// is parsed so the UI can render [i/N] without leaking resolver detail.
func ParseAll(paths []string, store *lockfile.State) []checks.ParsedWorkflow {
	total := len(paths)
	out := make([]checks.ParsedWorkflow, 0, total)
	for _, path := range paths {
		pw := checks.ParsedWorkflow{Path: path}
		wf, err := workflowfile.Load(path)
		if err != nil {
			pw.LoadErr = err
			out = append(out, pw)
			continue
		}
		scan := wf.ExtractActionRefs()
		selfScan := workflowfile.ScanSelfRepositoryActions(path, scan.SelfRepositoryActionRefs)

		pw.Refs = mergeActionRefs(scan.Refs, selfScan.Refs)
		// Refs inside `$/…` actions are rewritable too: their action file is
		// rewritten alongside the workflow, so narrowing stays in sync.
		pw.RewriteRefs = pw.Refs
		pw.SelfActionFiles = selfScan.ActionFiles
		pw.LocalPaths = mergeStrings(scan.LocalPaths, selfScan.LocalPaths)
		pw.SelfRepositoryRefs = mergeStrings(scan.SelfRepositoryRefs, selfScan.SelfRepositoryRefs)
		pw.SelfRepositoryRefErrs = mergeStrings(scan.SelfRepositoryRefErrs, selfScan.SelfRepositoryRefErrs)
		pw.SelfRepositoryResolutionErrs = selfScan.Errors
		pw.ParseWarnings = append(scan.Warnings, selfScan.Warnings...)
		if len(pw.Refs) > 0 && store != nil {
			wfKey := workflowfile.KeyFromPath(path)
			deps, depsErr := store.Get(wfKey)
			if depsErr != nil {
				pw.DepsErr = depsErr
			} else {
				pw.ExistingDeps = deps
			}
			closure, parents, closureErr := store.GetClosure(wfKey)
			if closureErr != nil {
				pw.DepsErr = closureErr
			} else {
				pw.RecordedDeps = closure
				pw.RecordedParents = parents
			}
		}
		out = append(out, pw)
	}
	return out
}

func mergeActionRefs(groups ...[]parserlock.ActionRef) []parserlock.ActionRef {
	seen := make(map[string]bool)
	var refs []parserlock.ActionRef
	for _, group := range groups {
		for _, ref := range group {
			key := ref.FullName() + "@" + ref.Ref
			if seen[key] {
				continue
			}
			seen[key] = true
			refs = append(refs, ref)
		}
	}
	return refs
}

func mergeStrings(groups ...[]string) []string {
	seen := make(map[string]bool)
	var values []string
	for _, group := range groups {
		for _, value := range group {
			if seen[value] {
				continue
			}
			seen[value] = true
			values = append(values, value)
		}
	}
	return values
}

// CollectResolvable returns the deduplicated current workflow roots across all
// parsed workflows. Recorded closure entries lack sub-action paths and must not
// become recursive discovery roots.
func CollectResolvable(parsed []checks.ParsedWorkflow) []parserlock.ActionRef {
	seenRef := make(map[ghapi.ActionRef]bool)
	var refs []parserlock.ActionRef
	for _, pw := range parsed {
		for _, ref := range pw.Refs {
			key := ghapi.ForActionRef(ref.Owner, ref.Repo, ref.Path, ref.Ref)
			if seenRef[key] {
				continue
			}
			seenRef[key] = true
			refs = append(refs, ref)
		}
	}
	return refs
}

// collectRecordedResolvable returns recorded closure refs that are not already
// represented by a path-aware current workflow root.
func collectRecordedResolvable(parsed []checks.ParsedWorkflow) []parserlock.ActionRef {
	seenRef := make(map[ghapi.NWORef]bool)
	for _, pw := range parsed {
		for _, ref := range pw.Refs {
			seenRef[ghapi.ForNWORef(ref.Owner, ref.Repo, ref.Ref)] = true
		}
	}

	var refs []parserlock.ActionRef
	for _, pw := range parsed {
		for _, d := range pw.RecordedDeps {
			owner, repo := d.OwnerRepo()
			key := ghapi.ForNWORef(owner, repo, d.Ref)
			if seenRef[key] {
				continue
			}
			seenRef[key] = true
			refs = append(refs, parserlock.ActionRef{
				Owner: owner,
				Repo:  repo,
				Ref:   d.Ref,
			})
		}
	}
	return refs
}

// DiagnoseParsed runs the engine diagnostics for each pre-parsed workflow.
// Assumes the resolver caches have already been warmed (calls into the
// resolver will hit cache and stay silent). Returns a checks.Report aggregating per-
// workflow findings in input order.
