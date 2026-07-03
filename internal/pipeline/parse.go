package pipeline

import (
	"context"
	"strings"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/dep"
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
		refs, _ := CollectResolvable(parsed)
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
		pw.Refs = scan.Refs
		pw.LocalPaths = scan.LocalPaths
		pw.SelfRepoRefs = scan.SelfRepoRefs
		pw.SelfRepoRefErrs = scan.SelfRepoRefErrs
		pw.JobLevelSelfRepoRefs = scan.JobLevelSelfRepoRefs
		pw.ParseWarnings = scan.Warnings
		if len(pw.Refs) > 0 {
			wfKey := workflowfile.KeyFromPath(path)
			deps, depsErr := store.Get(wfKey)
			if depsErr != nil {
				pw.DepsErr = depsErr
			} else {
				pw.ExistingDeps = deps
			}
		}
		out = append(out, pw)
	}
	return out
}

// CollectResolvable returns the deduplicated union of refs and existing deps
// across all parsed workflows. Use the returned slices to pre-warm the
// resolver caches once before per-workflow diagnostics.
func CollectResolvable(parsed []checks.ParsedWorkflow) ([]parserlock.ActionRef, []dep.Dependency) {
	return collectResolvable(parsed, nil)
}

// CollectUnrecordedResolvable is like CollectResolvable but excludes refs
// whose NWO@Ref key appears in recordedKeys. Deps whose key is in
// recordedKeys are also excluded. Use this when per-dep lockfile trust
// has already seeded the resolver cache for recorded deps, so only
// genuinely new refs need network resolution.
func CollectUnrecordedResolvable(parsed []checks.ParsedWorkflow, recordedKeys map[string]bool) ([]parserlock.ActionRef, []dep.Dependency) {
	return collectResolvable(parsed, recordedKeys)
}

func collectResolvable(parsed []checks.ParsedWorkflow, excludeKeys map[string]bool) ([]parserlock.ActionRef, []dep.Dependency) {
	seenRef := make(map[ghapi.ActionRef]bool)
	var refs []parserlock.ActionRef
	for _, pw := range parsed {
		for _, ref := range pw.Refs {
			nwoRef := strings.ToLower(ref.Owner+"/"+ref.Repo) + "@" + ref.Ref
			if excludeKeys[nwoRef] {
				continue
			}
			key := ghapi.ForActionRef(ref.Owner, ref.Repo, ref.Path, ref.Ref)
			if seenRef[key] {
				continue
			}
			seenRef[key] = true
			refs = append(refs, ref)
		}
	}
	seenDep := make(map[string]bool)
	var deps []dep.Dependency
	for _, pw := range parsed {
		for _, dep := range pw.ExistingDeps {
			key := dep.Key()
			if excludeKeys[key] || seenDep[key] {
				continue
			}
			seenDep[key] = true
			deps = append(deps, dep)
		}
	}
	return refs, deps
}

// DiagnoseParsed runs the engine diagnostics for each pre-parsed workflow.
// Assumes the resolver caches have already been warmed (calls into the
// resolver will hit cache and stay silent). Returns a checks.Report aggregating per-
// workflow findings in input order.
